// Package supervisor runs the per-job round loops: drives the daemon-native
// pi RPC client (internal/client), classifies exits, adaptive backoff, marker
// gate, instant-exit strikes.
package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"pi-supervisor/internal/client"
	"pi-supervisor/internal/events"
	"pi-supervisor/internal/job"
	"pi-supervisor/internal/stall"
)

// Handler is the control-socket surface (implemented by Supervisor).
type Handler interface {
	Status(job string) (any, error)
	Start(name string) error
	Stop(name string) error
	Steer(name, text string, noWait bool) (job.SteerReport, error)
	Logs(name string, n int) ([]string, error)
	Reload() error
}

// controlPollInterval is how often the client polls the ctrl file. Tests
// shrink it so a steer is delivered (and acked) in milliseconds; production
// uses the client's default.
var controlPollInterval = client.PollInterval

// steerWaitMargin is added to the poll interval to bound the ack wait: a
// frame is picked up within one poll, then acked in the same pass.
const steerWaitMargin = 2 * time.Second

type Supervisor struct {
	mu   sync.Mutex
	jobs map[string]*runner
	stop chan struct{}
	wg   sync.WaitGroup
	// steerWait overrides the ack-wait budget; 0 = poll interval + margin.
	steerWait time.Duration
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
		CIStalls: r.state.CIStalls,
		LastDiag: r.state.LastDiag, PRURL: r.state.PRURL,
		LastUpdate: time.Now().Format(time.RFC3339),
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

// Steer sends one control frame to a live round and reports what pi did with
// it (ADR-0005). Prose is wrapped in a prompt frame; a caller-supplied JSON
// frame is passed through unchanged. The write is only a request: the real
// outcome comes from the client's ack record for this frame id, and the wait
// for it holds no supervisor lock so the round loop keeps running.
func (s *Supervisor) Steer(name, text string, noWait bool) (job.SteerReport, error) {
	s.mu.Lock()
	r, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		return job.SteerReport{Job: name}, fmt.Errorf("unknown job %q", name)
	}
	frame, id, err := steerFrame(text)
	if err != nil {
		return job.SteerReport{Job: name}, err
	}

	// Snapshot under r.mu, then let go: nothing below may hold s.mu or r.mu
	// across a wait (lock order is s.mu -> r.mu, and the round loop needs
	// r.mu to publish its own end).
	r.mu.Lock()
	rep := job.SteerReport{
		Job:         name,
		FrameID:     id,
		Round:       r.state.Round,
		JobState:    r.state.State,
		SessionPath: r.job.SessionPath,
		CtrlPath:    job.Ctrl(name),
		AckPath:     job.Ack(name),
		LiveRound:   r.active && r.pid > 0,
		ClientPID:   r.pid,
	}
	r.mu.Unlock()

	if !rep.LiveRound {
		// Do not queue it: round() truncates the ctrl file at the start of
		// every round, so a frame written now would be wiped unread.
		rep.Outcome = job.AckNoRound
		rep.Detail = fmt.Sprintf("job state %q, round %d: no round is reading %s, so nothing was written",
			rep.JobState, rep.Round, rep.CtrlPath)
		return rep, fmt.Errorf("%s", rep.Detail)
	}

	// Remember where the ack log ends so only records the client writes for
	// THIS frame are considered (a restart reuses the same file).
	off := job.Size(rep.AckPath)
	if err := appendCtrl(rep.CtrlPath, frame); err != nil {
		rep.Outcome = job.AckSendFail
		rep.Detail = fmt.Sprintf("cannot write %s: %v", rep.CtrlPath, err)
		return rep, fmt.Errorf("%s", rep.Detail)
	}
	if noWait {
		rep.Outcome = job.AckWritten
		rep.Detail = "--no-wait: on disk, not confirmed; check the run log for \"[control] forwarded\""
		return rep, nil
	}
	s.waitAck(&rep, r, off)
	if !rep.Confirmed {
		return rep, fmt.Errorf("frame %s not confirmed: %s", rep.FrameID, rep.Outcome)
	}
	return rep, nil
}

// waitAck polls the ack log for this frame's terminal record. It never holds
// a supervisor lock, and it reports the round as gone rather than success
// when the round ends first.
func (s *Supervisor) waitAck(rep *job.SteerReport, r *runner, off int64) {
	wait := s.steerWait
	if wait <= 0 {
		wait = controlPollInterval + steerWaitMargin
	}
	start := time.Now()
	held := false
	for {
		for _, rec := range acksSince(rep.AckPath, off) {
			if rec.ID != rep.FrameID {
				continue
			}
			if rec.Outcome == job.AckHeld {
				// Accepted but not delivered yet: keep waiting for the
				// terminal record that follows the abort drain.
				held = true
				continue
			}
			rep.Outcome, rep.Detail, rep.DelayMS, rep.Confirmed = rec.Outcome, rec.Detail, rec.DelayMS, true
			rep.WaitedMS = time.Since(start).Milliseconds()
			if held {
				rep.Detail = strings.TrimSpace(rep.Detail + " (accepted while an abort drained, delivered after)")
			}
			return
		}
		if time.Since(start) >= wait {
			break
		}
		// The ack log is truncated with the ctrl file at every round start:
		// shrinking below our offset means this round ended.
		if job.Size(rep.AckPath) < off {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	rep.WaitedMS = time.Since(start).Milliseconds()
	rep.Outcome = job.AckUnconfirmed
	if r.live() {
		rep.Detail = fmt.Sprintf("no ack from pi within %s; the frame is on disk but unconfirmed", wait)
	} else {
		rep.Detail = fmt.Sprintf("round %d stopped reading %s before the frame was delivered; "+
			"the next round truncates it, so re-send after it starts", rep.Round, rep.CtrlPath)
	}
}

// acksSince decodes the ack records appended at or after offset.
func acksSince(path string, off int64) []job.AckRecord {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(off, 0); err != nil {
		return nil
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	var out []job.AckRecord
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec job.AckRecord
		if json.Unmarshal([]byte(line), &rec) == nil {
			out = append(out, rec)
		}
	}
	return out
}

// appendCtrl adds one LF-terminated JSON frame to the steer file.
func appendCtrl(path string, frame map[string]any) error {
	line, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

var steerSeq atomic.Uint64

// steerFrame turns operator text into one control frame. Prose becomes a
// prompt frame (the client's contract is strict JSON-LF); a caller-supplied
// JSON object with a "type" is passed through so operators can still send an
// abort or any other frame type by hand. Every frame gets an id so its ack
// record can be matched back.
func steerFrame(text string) (map[string]any, string, error) {
	body := strings.TrimSpace(text)
	if body == "" {
		return nil, "", errors.New("empty steer text")
	}
	var frame map[string]any
	if strings.HasPrefix(body, "{") && json.Unmarshal([]byte(body), &frame) == nil {
		if _, ok := frame["type"].(string); !ok {
			frame = nil // JSON but not a frame: treat it as prose
		}
	} else {
		frame = nil
	}
	if frame == nil {
		frame = map[string]any{"type": "prompt", "message": body}
	}
	id, _ := frame["id"].(string)
	if id == "" {
		id = fmt.Sprintf("steer-%d-%d", time.Now().UnixNano(), steerSeq.Add(1))
		frame["id"] = id
	}
	return frame, id, nil
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
			r.mu.Lock()
			name := r.job.Name
			r.mu.Unlock()
			r.finish("fatal", "round cap reached without marker")
			s.logf(name, "FATAL: round cap %d reached without marker", maxRounds)
			s.emit(name, "fatal", round, 0, 0, "", "round cap %d reached without marker", maxRounds)
			return
		}
		r.mu.Lock()
		r.state.Round = round
		j := r.job
		r.mu.Unlock()
		if round == 1 {
			s.emit(j.Name, "job_started", round, 0, 0, "", "round 1 launched")
		}

		// CI-stall watcher (ADR-0004): rounds with a captured transcript get
		// a detector that interrupts the session and fails the run once the
		// agent has parked on the CI/review loop ci_stall_cap times.
		r.mu.Lock()
		sess := r.job.SessionPath
		r.mu.Unlock()
		var rc int
		var dur, runlogB int64
		if sess != "" {
			watchStop, watchExited := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(watchExited)
				s.watchCIStalls(r, sess, round, watchStop, stopCh)
			}()
			rc, dur, runlogB = s.round(r, round, stopCh)
			close(watchStop)
			<-watchExited // watcher stopped before the round is classified
		} else {
			rc, dur, runlogB = s.round(r, round, stopCh)
		}
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

		snap := r.stateSnapshot()
		roundText := snap.LastDiag
		if snap.PRURL != "" {
			roundText += " | pr " + snap.PRURL
		}
		s.emit(name, "round_done", round, rc, dur, roundText, "")
		if instant {
			s.emit(name, "instant_exit", round, rc, dur, "", "instant exit strike %d/3 (rc=%d, %ds)", strikes, rc, dur)
		}
		s.logf(name, "round %d: client exit=%d duration=%ds runlog=%dB", round, rc, dur, runlogB)
		if r.stateSnapshot().LastDiag != "" {
			s.logf(name, "round %d diagnostic: %s", round, r.stateSnapshot().LastDiag)
		}

		// The marker gate. An empty marker would make RunlogContains match
		// ANY non-empty run log (strings.Contains(x, "") is true), so a job
		// configured without a marker could be declared done by a stale final
		// report. Refuse: no marker, no done.
		if j.Marker != "" && job.Exists(j.FinalReport) && job.RunlogContains(job.Runlog(name), j.Marker) {
			r.mu.Lock()
			r.state.State, r.active = "done", false
			r.mu.Unlock()
			r.persistState()
			s.logf(name, "round %d: marker %q detected — done", round, j.Marker)
			s.emit(name, "done", round, 0, dur, "", "marker %q detected — run is over", j.Marker)
			return
		}

		// A CI-stall cap intervention may have closed the run mid-round
		// (ADR-0004). The done-check above already had its chance to upgrade
		// a finished report to done; otherwise the fatal stands.
		if r.stateSnapshot().State == "fatal" {
			s.logf(name, "round %d: run closed by CI-stall intervention", round)
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
			adopted := r.job
			r.mu.Unlock()
			_ = job.Save(adopted)
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
		r.mu.Lock()
		scale := r.job.BackoffScale
		r.mu.Unlock()
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

// live reports whether a round is currently polling the control file.
func (r *runner) live() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active && r.pid > 0
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
	// Fresh steer channel per round: the client reads the ctrl file from its
	// start, so a leftover frame from the previous round would replay into
	// this one. The ack log is truncated with it (ADR-0005).
	if err := os.WriteFile(job.Ctrl(j.Name), nil, 0o644); err != nil {
		s.logf(j.Name, "round %d: cannot reset steer channel %s: %v", round, job.Ctrl(j.Name), err)
		return 1, 0, 0
	}
	if err := os.WriteFile(job.Ack(j.Name), nil, 0o644); err != nil {
		s.logf(j.Name, "round %d: cannot reset ack log %s: %v", round, job.Ack(j.Name), err)
	}

	if resume {
		s.logf(j.Name, "round %d: RESUME %s", round, j.SessionPath)
	} else {
		s.logf(j.Name, "round %d: LAUNCH brief=%s", round, j.Brief)
	}

	out, err := os.OpenFile(runlog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 1, 0, 0
	}
	// The run log is a diagnostic mirror; the round's verdict comes from
	// the client Result, so a failed close (append-only handle) is not fatal.
	defer func() { _ = out.Close() }()

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
		PiBin:        j.PiBin,
		Session:      j.SessionPath,
		Name:         j.SessionName,
		Worktree:     j.Worktree,
		Skills:       j.Skills,
		Provider:     j.Provider,
		Model:        j.Model,
		Prompt:       prompt,
		ControlPath:  job.Ctrl(j.Name),
		AckPath:      job.Ack(j.Name),
		PollInterval: controlPollInterval,
		Timeout:      time.Duration(j.TimeoutS) * time.Second,
		GracePeriod:  30 * time.Second,
		Out:          out,
		Diag:         out,
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

// watchCIStalls tails the session transcript for one round and intervenes at
// the stall cap (ADR-0004). Non-cap stalls only emit; the cap stall writes
// abort+prompt frames telling the agent to finish the report, then closes the
// run as a failure to finish the review loop. The round itself is left alive
// so the report turn can actually run.
func (s *Supervisor) watchCIStalls(r *runner, sess string, round int, watchStop, stopCh chan struct{}) {
	r.mu.Lock()
	capN, idleS, name := r.job.CIStallCap, r.job.CIStallIdleS, r.job.Name
	r.mu.Unlock()
	if capN <= 0 {
		capN = 3
	}
	if idleS <= 0 {
		idleS = 300
	}
	d := stall.New(sess, time.Duration(idleS)*time.Second)
	tick := time.Duration(idleS) / 10
	if tick < 200*time.Millisecond {
		tick = 200 * time.Millisecond
	}
	if tick > 15*time.Second {
		tick = 15 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	// Final scan on every exit path: the last lines of a round's transcript
	// (where the agent links its PR) are written as the client exits, so the
	// regular tick can miss them. Without this the PR URL would only surface
	// on the round AFTER the one that opened it.
	defer func() {
		d.Poll() // final verdict is irrelevant; only the PR scrape matters
		s.recordPR(r, d.PRURL())
	}()
	for {
		select {
		case <-watchStop:
			return
		case <-stopCh:
			return
		case <-t.C:
			stalled, marker := d.Poll()
			s.recordPR(r, d.PRURL())
			if !stalled {
				continue
			}
			r.mu.Lock()
			r.state.CIStalls++
			n := r.state.CIStalls
			r.mu.Unlock()
			s.logf(name, "round %d: CI/review stall %d/%d (%s)", round, n, capN, marker)
			s.emit(name, "ci_stall", round, 0, 0, "",
				"stall %d/%d — agent parked on CI/review: %s", n, capN, marker)
			if n < capN {
				continue
			}
			// Cap reached: instruct the agent to finish the report, then
			// close the run as a review-loop failure. Keep the round alive
			// so the abort+prompt turn can actually deliver the report.
			interrupt := "CI/review stall cap reached (" + fmt.Sprint(capN) +
				" parks with no transcript progress). Stop polling CI. Finish the review " +
				"report NOW: summarize what landed, what failed, and what the operator " +
				"must check. The supervisor is closing this run as a failure to finish " +
				"the review loop."
			interruptErr := s.interruptWith(name, interrupt)
			if interruptErr != nil {
				// The round can no longer be told to finish the report, so the
				// agent will be killed without it — surface that instead of
				// silently reporting "agent instructed".
				s.logf(name, "ERROR: CI-stall cap interrupt not delivered: %v", interruptErr)
			}
			delivered := ""
			if interruptErr == nil {
				delivered = "; agent instructed to finish the report"
			} else {
				delivered = "; FINISH-THE-REPORT INTERRUPT NOT DELIVERED (" +
					interruptErr.Error() + ")"
			}
			r.mu.Lock()
			r.state.State, r.state.LastDiag, r.active = "fatal",
				"CI review retry cap exceeded — review loop did not finish"+delivered, false
			r.mu.Unlock()
			r.persistState()
			s.logf(name, "FATAL: CI review retry cap %d exceeded%s", capN, delivered)
			s.emit(name, "fatal", round, 0, 0, "",
				"CI review retry cap %d exceeded — review loop did not finish%s", capN, delivered)
			return
		}
	}
}

// recordPR stores the first pull-request URL seen in the round's transcript
// on the runner (ADR-0006). Best-effort and eventually consistent: an empty
// url means "not linked", never "no PR exists". Lock order stays s.mu -> r.mu:
// this is called from the watcher goroutine with neither held.
func (s *Supervisor) recordPR(r *runner, url string) {
	if url == "" {
		return
	}
	r.mu.Lock()
	fresh := r.state.PRURL != url
	if fresh {
		r.state.PRURL = url
	}
	name := r.job.Name
	r.mu.Unlock()
	if fresh {
		s.logf(name, "pull request: %s", url)
	}
}

// interruptWith delivers an instant abort+fresh-prompt pair to a live round's
// control file — the same wire format pi_control.py --interrupt writes and
// the client's abort-drain handshake consumes.
func (s *Supervisor) interruptWith(name, text string) error {
	f, err := os.OpenFile(job.Ctrl(name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close() // control file: the frames are already written below
	rid := fmt.Sprintf("%d", time.Now().UnixNano())
	for _, frame := range []any{
		map[string]any{"id": "abort-" + rid, "type": "abort"},
		map[string]any{"id": "int-" + rid, "type": "prompt", "message": text},
	} {
		line, _ := json.Marshal(frame)
		if _, err := f.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// emit records one lifecycle event (audit JSONL + live fan-out to watches).
// info is printf-formatted for readable diagnostics. The job's worktree is
// attached so a waking LLM can target verification at the right path.
func (s *Supervisor) emit(jobName, event string, round, rc int, durS int64, text, format string, args ...any) {
	if len(text) > 200 {
		text = text[len(text)-200:]
	}
	info := format
	if len(args) > 0 {
		info = fmt.Sprintf(format, args...)
	}
	var wt, sess, pr string
	s.mu.Lock()
	if r, ok := s.jobs[jobName]; ok {
		r.mu.Lock()
		wt = r.job.Worktree
		sess = r.job.SessionPath
		pr = r.state.PRURL
		r.mu.Unlock()
	}
	s.mu.Unlock()
	events.Emit(events.Event{
		Job: jobName, Event: event, Round: round, RC: rc, DurS: durS,
		Text: text, Info: info, Worktree: wt, SessionPath: sess, PRURL: pr,
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
				r.mu.Lock()
				ev.Worktree = r.job.Worktree
				ev.SessionPath = r.job.SessionPath
				ev.PRURL = r.state.PRURL
				r.mu.Unlock()
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

// Shutdown terminates all active round loops: it closes each runner's stop
// channel (so the loop's stop path runs and no NEW round is spawned) and
// SIGTERMs the live client groups, then waits for the loops to unwind.
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
		if active {
			// Same guarded close as Stop(): a concurrent operator Stop() must
			// not turn this into a double-close of the channel.
			r.active = false
			close(r.stopCh)
		}
		r.mu.Unlock()
		if active && pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGTERM)
		}
	}
	s.wg.Wait()
}
