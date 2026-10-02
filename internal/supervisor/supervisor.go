// Package supervisor runs the per-job round loops: drives the daemon-native
// pi RPC client (internal/client), classifies exits, adaptive backoff, marker
// gate, instant-exit strikes.
package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"pi-supervisor/internal/client"
	"pi-supervisor/internal/events"
	"pi-supervisor/internal/job"
)

// Handler is the control-socket surface (implemented by Supervisor).
type Handler interface {
	Status(job string) (any, error)
	Start(name string) error
	Stop(name string) error
	Steer(name, text string) error
	Logs(name string, n int) ([]string, error)
	Reload() error
}

type Supervisor struct {
	mu   sync.Mutex
	jobs map[string]*runner
	stop chan struct{}
	wg   sync.WaitGroup
}

type runner struct {
	mu      sync.Mutex
	job     job.Job
	state   job.State
	pid     int
	started time.Time
	runlogB int64
	stopCh  chan struct{}
	active  bool
}

func New() *Supervisor {
	return &Supervisor{jobs: map[string]*runner{}, stop: make(chan struct{})}
}

func (s *Supervisor) logf(name, format string, args ...any) {
	_, _, orchlog, _ := job.Paths(name)
	f, err := os.OpenFile(orchlog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// LoadJobs (re)reads the jobs dir; existing runners get hot-reloaded config.
func (s *Supervisor) LoadJobs() error {
	entries, err := os.ReadDir(job.JobsDir())
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		j, err := job.Load(filepath.Join(job.JobsDir(), e.Name()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad job %s: %v\n", e.Name(), err)
			continue
		}
		seen[j.Name] = true
		if r, ok := s.jobs[j.Name]; ok {
			r.mu.Lock()
			r.job = j
			r.mu.Unlock()
			continue
		}
		r := &runner{job: j, stopCh: make(chan struct{})}
		if st, err := job.LoadState(j.Name); err == nil {
			r.state = st
			if r.state.State == "running" {
				r.state.State = "stopped" // adopt as resumable, not auto-running
			}
		}
		s.jobs[j.Name] = r
	}
	for name := range s.jobs {
		if !seen[name] {
			delete(s.jobs, name)
		}
	}
	return nil
}

func (r *runner) persistState() {
	r.mu.Lock()
	st, name := r.state, r.job.Name
	r.mu.Unlock()
	_ = job.SaveState(name, st)
}

func (r *runner) snapshot() job.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := job.Status{
		Name: r.job.Name, State: r.state.State, Round: r.state.Round,
		MaxRounds: r.job.MaxRounds, SessionPath: r.job.SessionPath,
		ClientPID: r.pid, LastRC: r.state.LastRC, LastDurS: r.state.LastDurS,
		LastRunlogB: r.runlogB, InstantExits: r.state.InstantExits,
		LastDiag: r.state.LastDiag, LastUpdate: time.Now().Format(time.RFC3339),
	}
	if r.job.FinalReport != "" {
		st.FinalReportOK = job.Exists(r.job.FinalReport)
	}
	if r.job.SessionPath != "" {
		if fi, err := os.Stat(r.job.SessionPath); err == nil {
			st.SessionBytes = fi.Size()
			st.SessionAgeS = time.Since(fi.ModTime()).Seconds()
		}
	}
	st.MarkerFound = r.state.State == "done"
	return st
}

func (s *Supervisor) Status(name string) (any, error) {
	s.mu.Lock()
	r, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown job %q", name)
	}
	return r.snapshot(), nil
}

// StatusAll returns snapshots for every job, sorted by name.
func (s *Supervisor) StatusAll() any {
	s.mu.Lock()
	names := make([]string, 0, len(s.jobs))
	byName := make(map[string]*runner, len(s.jobs))
	for n, r := range s.jobs {
		names = append(names, n)
		byName[n] = r
	}
	s.mu.Unlock()
	sort.Strings(names)
	out := make([]job.Status, 0, len(names))
	for _, n := range names {
		out = append(out, byName[n].snapshot())
	}
	return out
}

func (s *Supervisor) Start(name string) error {
	s.mu.Lock()
	r, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown job %q", name)
	}
	r.mu.Lock()
	if r.active {
		r.mu.Unlock()
		return fmt.Errorf("job %q already running", name)
	}
	if r.state.State == "done" {
		r.mu.Unlock()
		return fmt.Errorf("job %q already done; clear state to rerun", name)
	}
	r.active = true
	r.state.State = "running"
	r.state.InstantExits = 0
	if r.state.StartedAt == "" {
		r.state.StartedAt = time.Now().Format(time.RFC3339)
	}
	// Fresh stop channel every start: Stop() closed the previous one, and a
	// closed channel would make the new loop exit instantly.
	stopCh := make(chan struct{})
	r.stopCh = stopCh
	r.mu.Unlock()
	r.persistState()

	s.wg.Go(func() {
		s.loop(r, stopCh)
	})
	return nil
}

func (s *Supervisor) Stop(name string) error {
	s.mu.Lock()
	r, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown job %q", name)
	}
	r.mu.Lock()
	active := r.active
	if active {
		// Mark inactive synchronously under the lock so a second Stop() is a
		// no-op rather than a double-close of the stop channel.
		r.active = false
		close(r.stopCh)
	}
	pid := r.pid
	r.mu.Unlock()
	if !active {
		return fmt.Errorf("job %q not running", name)
	}
	s.logf(name, "stop requested by operator (pid %d)", pid)
	if pid <= 0 {
		// The client may not have published its pid yet (still spawning).
		// The round goroutine's stop path escalates on its own; nothing to
		// signal here.
		return nil
	}
	// Kill the whole process group, then escalate if it lingers. Bounded so
	// Stop() never returns while pi is still alive (an orphan pi would keep
	// writing to the session JSONL unsupervised).
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		gone := r.pid == 0
		r.mu.Unlock()
		if gone {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return nil
}

func (s *Supervisor) Steer(name, text string) error {
	s.mu.Lock()
	_, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown job %q", name)
	}
	f, err := os.OpenFile(job.Ctrl(name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.TrimRight(text, "\n") + "\n")
	return err
}

func (s *Supervisor) Logs(name string, n int) ([]string, error) {
	if n <= 0 {
		n = 30
	}
	return job.LastLines(job.Runlog(name), n)
}

// Reload rescans the jobs dir (hot config).
func (s *Supervisor) Reload() error { return s.LoadJobs() }

// loop is the round loop for one job.
func (s *Supervisor) loop(r *runner, stopCh chan struct{}) {
	for {
		r.mu.Lock()
		round := r.state.Round + 1
		maxRounds := r.job.MaxRounds
		r.mu.Unlock()
		if round > maxRounds {
			r.finish("fatal", "round cap reached without marker")
			s.logf(r.job.Name, "FATAL: round cap %d reached without marker", maxRounds)
			s.emit(r.job.Name, "fatal", round, 0, 0, "", "round cap %d reached without marker", maxRounds)
			return
		}
		r.mu.Lock()
		r.state.Round = round
		j := r.job
		r.mu.Unlock()
		if round == 1 {
			s.emit(j.Name, "job_started", round, 0, 0, "", "round 1 launched")
		}

		rc, dur, runlogB := s.round(r, round, stopCh)
		select {
		case <-stopCh:
			r.mu.Lock()
			r.state.State, r.state.LastRC, r.active = "stopped", rc, false
			name := r.job.Name
			r.mu.Unlock()
			r.persistState()
			s.logf(name, "loop stopped at round %d (client rc=%d)", round, rc)
			s.emit(name, "stopped", round, rc, dur, "", "operator stop")
			return
		default:
		}

		r.mu.Lock()
		r.state.LastRC, r.state.LastDurS = rc, dur
		name := r.job.Name
		instant := rc != 0 && dur < 60 && runlogB < 4096
		if instant {
			r.state.InstantExits++
		} else if rc == 0 || dur >= 1700 {
			r.state.InstantExits = 0
		}
		strikes := r.state.InstantExits
		r.state.LastDiag = job.Tail(job.Runlog(name), 240)
		r.mu.Unlock()

		s.emit(name, "round_done", round, rc, dur, r.stateSnapshot().LastDiag, "")
		if instant {
			s.emit(name, "instant_exit", round, rc, dur, "", "instant exit strike %d/3 (rc=%d, %ds)", strikes, rc, dur)
		}
		s.logf(name, "round %d: client exit=%d duration=%ds runlog=%dB", round, rc, dur, runlogB)
		if r.stateSnapshot().LastDiag != "" {
			s.logf(name, "round %d diagnostic: %s", round, r.stateSnapshot().LastDiag)
		}

		if job.Exists(j.FinalReport) && job.RunlogContains(job.Runlog(name), j.Marker) {
			r.mu.Lock()
			r.state.State, r.active = "done", false
			r.mu.Unlock()
			r.persistState()
			s.logf(name, "round %d: marker %q detected — done", round, j.Marker)
			s.emit(name, "done", round, 0, dur, "", "marker %q detected — run is over", j.Marker)
			return
		}

		if instant && strikes >= 3 {
			r.finish("fatal", "3 consecutive instant exits — likely context exhaustion or model refusal")
			s.logf(name, "FATAL: 3 consecutive instant exits — manual intervention required")
			s.emit(name, "fatal", round, rc, dur, "", "3 consecutive instant exits — likely context exhaustion or model refusal")
			return
		}

		// First LAUNCH round must capture the session path (never re-LAUNCH:
		// a second launch forks the session).
		r.mu.Lock()
		pinned := r.job.SessionPath != ""
		r.mu.Unlock()
		if !pinned {
			p := job.FindSession(j.SessionName, j.Worktree)
			if p == "" {
				r.finish("fatal", "could not capture session path after LAUNCH — refusing to fork a new session")
				s.logf(name, "FATAL: could not capture session path after LAUNCH — aborting instead of forking")
				s.emit(name, "fatal", round, 0, dur, "", "could not capture session path — refusing to fork")
				return
			}
			r.mu.Lock()
			r.job.SessionPath = p
			r.mu.Unlock()
			_ = job.Save(r.job)
			s.logf(name, "round %d: captured session path %s", round, p)
		}

		// Adaptive backoff: a clean cap means pi is healthy and progressing,
		// an instant implosion means something is wrong and retrying fast just
		// burns the context wall. Scale lets tests and operators tune it.
		var sleep time.Duration
		switch {
		case rc == 0:
			sleep = 15 * time.Second
		case dur >= 1700:
			sleep = 20 * time.Second
		case instant:
			sleep = 90 * time.Second
		default:
			sleep = 30 * time.Second
		}
		scale := r.job.BackoffScale
		if scale <= 0 {
			scale = 1.0
		}
		sleep = time.Duration(float64(sleep) * scale)
		s.emit(name, "backoff", round, rc, dur, "", "sleeping %s before round %d", sleep.Round(time.Second), round+1)
		select {
		case <-stopCh:
			r.mu.Lock()
			r.state.State, r.active = "stopped", false
			r.mu.Unlock()
			r.persistState()
			s.emit(name, "stopped", round, rc, dur, "", "operator stop during backoff")
			return
		case <-time.After(sleep):
		}
	}
}

func (r *runner) stateSnapshot() job.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *runner) finish(state, diag string) {
	r.mu.Lock()
	r.state.State, r.state.LastDiag, r.active = state, diag, false
	name := r.job.Name
	r.mu.Unlock()
	r.persistState()
	_ = name
}

// round runs one pi RPC round via the daemon-native client (internal/client);
// hard-kills the pi process group at timeout+120s as a backstop.
func (s *Supervisor) round(r *runner, round int, stopCh chan struct{}) (rc int, dur int64, runlogB int64) {
	r.mu.Lock()
	j := r.job
	resume := j.SessionPath != ""
	r.mu.Unlock()

	_, runlog, _, _ := job.Paths(j.Name)
	_ = os.Truncate(runlog, 0)
	_ = os.WriteFile(job.Ctrl(j.Name), nil, 0o644) // fresh steer channel per round

	if resume {
		s.logf(j.Name, "round %d: RESUME %s", round, j.SessionPath)
	} else {
		s.logf(j.Name, "round %d: LAUNCH brief=%s", round, j.Brief)
	}

	out, err := os.OpenFile(runlog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 1, 0, 0
	}
	defer out.Close()

	promptPath := j.Brief
	if resume {
		promptPath = j.Cont
	}
	prompt, err := client.DefaultPromptFile(promptPath)
	if err != nil {
		s.logf(j.Name, "round %d: prompt unreadable: %v", round, err)
		return 1, 0, 0
	}

	opts := client.Options{
		PiBin:       j.PiBin,
		Session:     j.SessionPath,
		Name:        j.SessionName,
		Worktree:    j.Worktree,
		Skills:      j.Skills,
		Provider:    j.Provider,
		Model:       j.Model,
		Prompt:      prompt,
		ControlPath: job.Ctrl(j.Name),
		Timeout:     time.Duration(j.TimeoutS) * time.Second,
		GracePeriod: 30 * time.Second,
		Out:         out,
		Diag:        out,
		OnPID: func(pid int) {
			r.mu.Lock()
			r.pid = pid
			r.started = time.Now()
			r.mu.Unlock()
		},
	}

	type outcome struct {
		res client.Result
	}
	doneCh := make(chan outcome, 1)
	start := time.Now()
	go func() {
		doneCh <- outcome{res: client.Run(opts)}
	}()

	backstop := time.NewTimer(time.Duration(j.TimeoutS+120) * time.Second)
	defer backstop.Stop()

	clearPID := func() {
		r.mu.Lock()
		r.pid = 0
		r.mu.Unlock()
	}

	select {
	case o := <-doneCh:
		clearPID()
		if o.res.Err != "" {
			s.logf(j.Name, "round %d: client error: %s", round, o.res.Err)
		}
		return o.res.RC, int64(time.Since(start).Seconds()), job.Size(runlog)

	case <-backstop.C:
		// Last-resort net: the client owns its own soft deadline, so this only
		// fires if the client goroutine itself wedged.
		r.mu.Lock()
		pid := r.pid
		r.mu.Unlock()
		if pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		s.logf(j.Name, "round %d: hard kill after %ds (supervisor backstop)", round, int(time.Since(start).Seconds()))
		o := <-doneCh
		clearPID()
		return o.res.RC, int64(time.Since(start).Seconds()), job.Size(runlog)

	case <-stopCh:
		s.logf(j.Name, "round %d: operator stop — terminating pi process group", round)
		// Escalate fast and deterministically so an operator stop (and test
		// teardown) never leaves an orphan pi writing to the session:
		// SIGTERM the group, then SIGKILL after a short grace. This mirrors
		// Stop() but also covers the case where Stop() fired before OnPID
		// published the pid (pid==0 there), which is exactly when an
		// unsupervised pi would otherwise survive.
		r.mu.Lock()
		spid := r.pid
		r.mu.Unlock()
		if spid > 0 {
			_ = syscall.Kill(-spid, syscall.SIGTERM)
		}
		done := make(chan struct{})
		go func() {
			o := <-doneCh
			clearPID()
			doneCh <- o
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			r.mu.Lock()
			pid := r.pid
			r.mu.Unlock()
			if pid > 0 {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
			<-done
		}
		o := <-doneCh
		return o.res.RC, int64(time.Since(start).Seconds()), job.Size(runlog)
	}
}

// emit records one lifecycle event (audit JSONL + live fan-out to watches).
// info is printf-formatted for readable diagnostics.
func (s *Supervisor) emit(jobName, event string, round, rc int, durS int64, text, format string, args ...any) {
	if len(text) > 200 {
		text = text[len(text)-200:]
	}
	info := format
	if len(args) > 0 {
		info = fmt.Sprintf(format, args...)
	}
	events.Emit(events.Event{
		Job: jobName, Event: event, Round: round, RC: rc, DurS: durS,
		Text: text, Info: info,
	})
}

// Watch subscribes the caller to a job's lifecycle events ("" = all jobs).
// If the requested job is already terminal (done|fatal|stopped) it returns a
// non-nil precheck Event instead of a channel: the server sends it and closes
// rather than blocking on a finished run (ADR-0003, requirement 4).
func (s *Supervisor) Watch(jobName string) (<-chan events.Event, func(), *events.Event) {
	id, ch, cancel := events.Subscribe()
	if jobName != "" {
		s.mu.Lock()
		r, ok := s.jobs[jobName]
		s.mu.Unlock()
		if ok {
			snap := r.snapshot()
			switch snap.State {
			case "done", "fatal", "stopped":
				cancel()
				ev := events.Event{
					TS: time.Now().UTC().Format(time.RFC3339), Job: jobName,
					Event: snap.State, Round: snap.Round, RC: snap.LastRC,
					DurS: snap.LastDurS,
					Info: "run already " + snap.State + " — nothing to wait for",
				}
				return nil, func() {}, &ev
			}
		}
	}
	_ = id
	return ch, cancel, nil
}

// RunningCount reports how many jobs are currently in a running round. Fed
// to systemd as STATUS= on every watchdog beat.
func (s *Supervisor) RunningCount() int {
	s.mu.Lock()
	runners := make([]*runner, 0, len(s.jobs))
	for _, r := range s.jobs {
		runners = append(runners, r)
	}
	s.mu.Unlock()
	n := 0
	for _, r := range runners {
		r.mu.Lock()
		if r.active && r.state.State == "running" {
			n++
		}
		r.mu.Unlock()
	}
	return n
}

// statusList is the typed StatusAll used internally by Monitor.
func (s *Supervisor) statusList() []job.Status {
	s.mu.Lock()
	names := make([]string, 0, len(s.jobs))
	byName := make(map[string]*runner, len(s.jobs))
	for n, r := range s.jobs {
		names = append(names, n)
		byName[n] = r
	}
	s.mu.Unlock()
	sort.Strings(names)
	out := make([]job.Status, 0, len(names))
	for _, n := range names {
		out = append(out, byName[n].snapshot())
	}
	return out
}

// Monitor refreshes per-job status JSON files every 2s.
func (s *Supervisor) Monitor(stop chan struct{}) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			for _, st := range s.statusList() {
				_, _, _, statusPath := job.Paths(st.Name)
				data, _ := json.MarshalIndent(st, "", "  ")
				_ = os.WriteFile(statusPath, data, 0o644)
			}
		}
	}
}

// Shutdown terminates all active round loops (SIGTERM to client groups).
func (s *Supervisor) Shutdown() {
	close(s.stop)
	s.mu.Lock()
	runners := make([]*runner, 0, len(s.jobs))
	for _, r := range s.jobs {
		runners = append(runners, r)
	}
	s.mu.Unlock()
	for _, r := range runners {
		r.mu.Lock()
		active, pid := r.active, r.pid
		r.mu.Unlock()
		if active && pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGTERM)
		}
	}
	s.wg.Wait()
}
