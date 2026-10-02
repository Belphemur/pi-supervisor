// Package supervisor runs the per-job round loops: spawn pi_rpc_client.py,
// classify exits, adaptive backoff, marker gate, instant-exit strikes.
package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(r, stopCh)
	}()
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
		close(r.stopCh)
	}
	pid := r.pid
	r.mu.Unlock()
	if !active {
		return fmt.Errorf("job %q not running", name)
	}
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGTERM) // whole process group
	}
	s.logf(name, "stop requested by operator (pid %d)", pid)
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
			return
		}
		r.mu.Lock()
		r.state.Round = round
		j := r.job
		r.mu.Unlock()

		rc, dur, runlogB := s.round(r, round, stopCh)
		select {
		case <-stopCh:
			r.mu.Lock()
			r.state.State, r.state.LastRC, r.active = "stopped", rc, false
			name := r.job.Name
			r.mu.Unlock()
			r.persistState()
			s.logf(name, "loop stopped at round %d (client rc=%d)", round, rc)
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
			return
		}

		if instant && strikes >= 3 {
			r.finish("fatal", "3 consecutive instant exits — likely context exhaustion or model refusal")
			s.logf(name, "FATAL: 3 consecutive instant exits — manual intervention required")
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
				return
			}
			r.mu.Lock()
			r.job.SessionPath = p
			r.mu.Unlock()
			_ = job.Save(r.job)
			s.logf(name, "round %d: captured session path %s", round, p)
		}

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
		select {
		case <-stopCh:
			r.mu.Lock()
			r.state.State, r.active = "stopped", false
			r.mu.Unlock()
			r.persistState()
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

// round spawns one pi_rpc_client.py round; hard-kills at timeout+120s.
func (s *Supervisor) round(r *runner, round int, stopCh chan struct{}) (rc int, dur int64, runlogB int64) {
	r.mu.Lock()
	j := r.job
	resume := j.SessionPath != ""
	r.mu.Unlock()

	_, runlog, _, _ := job.Paths(j.Name)
	_ = os.Truncate(runlog, 0)
	_ = os.WriteFile(job.Ctrl(j.Name), nil, 0o644) // fresh steer channel per round

	args := []string{filepath.Join(job.PiScripts(), "pi_rpc_client.py")}
	if resume {
		args = append(args, "--session", j.SessionPath)
	} else {
		args = append(args, "-C", j.Worktree, "-n", j.SessionName)
	}
	for _, sk := range j.Skills {
		args = append(args, "--skill", sk)
	}
	prompt := j.Brief
	if resume {
		prompt = j.Cont
	}
	args = append(args, "--control", job.Ctrl(j.Name), "-f", prompt, "--timeout", strconv.Itoa(j.TimeoutS))

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

	cmd := exec.Command("python3", args...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		s.logf(j.Name, "round %d: spawn failed: %v", round, err)
		return 1, 0, 0
	}
	r.mu.Lock()
	r.pid = cmd.Process.Pid
	r.started = start
	r.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(time.Duration(j.TimeoutS+120) * time.Second)
	defer timer.Stop()

	select {
	case err := <-done:
		r.mu.Lock()
		r.pid = 0
		r.mu.Unlock()
		if err == nil {
			return 0, int64(time.Since(start).Seconds()), job.Size(runlog)
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), int64(time.Since(start).Seconds()), job.Size(runlog)
		}
		return 1, int64(time.Since(start).Seconds()), job.Size(runlog)
	case <-timer.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		s.logf(j.Name, "round %d: hard kill after %ds (timeout watchdog)", round, int(time.Since(start).Seconds()))
		<-done
		r.mu.Lock()
		r.pid = 0
		r.mu.Unlock()
		return 137, int64(time.Since(start).Seconds()), job.Size(runlog)
	case <-stopCh:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		r.mu.Lock()
		r.pid = 0
		r.mu.Unlock()
		return 143, int64(time.Since(start).Seconds()), job.Size(runlog)
	}
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
