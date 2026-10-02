package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// Guard rails on the control surface: unknown jobs, double start, start of a
// finished run, and steering an unknown job.
func TestControlSurfaceGuards(t *testing.T) {
	testEnv(t)
	writeJob(t, job.Job{
		Name: "g", Brief: "/tmp/x.md", Worktree: t.TempDir(),
		SessionName: "g", MaxRounds: 1, TimeoutS: 5, PiBin: "true",
	})
	s := newTestSupervisor(t)

	if _, err := s.Status("nope"); err == nil {
		t.Fatal("Status of an unknown job must error")
	}
	if err := s.Start("nope"); err == nil {
		t.Fatal("Start of an unknown job must error")
	}
	if err := s.Stop("nope"); err == nil {
		t.Fatal("Stop of an unknown job must error")
	}
	if err := s.Steer("nope", "x"); err == nil {
		t.Fatal("Steer of an unknown job must error")
	}
	if all, ok := s.StatusAll().([]job.Status); !ok || len(all) != 1 || all[0].Name != "g" {
		t.Fatalf("StatusAll = %#v", s.StatusAll())
	}
	// Stop on a job that was never started is a no-op error, not a crash.
	if err := s.Stop("g"); err == nil {
		t.Fatal("Stop of an idle job must report not running")
	}
}

// A run that already reached its marker refuses to restart until the operator
// clears the state (no silent re-run of a finished campaign).
func TestStartRefusesFinishedJob(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)
	dir := t.TempDir()
	report := filepath.Join(dir, "final.md")
	if err := os.WriteFile(report, []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_STREAM MARKER_ALL_DONE"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "finished", Brief: brief, Worktree: t.TempDir(), SessionName: "finished",
		FinalReport: report, Marker: "MARKER_ALL_DONE",
		MaxRounds: 2, TimeoutS: 20, PiBin: pi, BackoffScale: 0.02,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := newTestSupervisor(t)
	if err := s.Start("finished"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, func() bool {
		st, _ := s.Status("finished")
		return st.(job.Status).State == "done"
	})
	if err := s.Start("finished"); err == nil || !strings.Contains(err.Error(), "already done") {
		t.Fatalf("Start after done = %v, want a refusal", err)
	}
}

// An empty marker must NOT be able to close a run: strings.Contains(x, "")
// is true, so a stale final report alone used to end the job.
func TestEmptyMarkerCannotFinishJob(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)
	dir := t.TempDir()
	report := filepath.Join(dir, "final.md")
	if err := os.WriteFile(report, []byte("stale report from a previous run"), 0o644); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_STREAM"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "nomarker", Brief: brief, Worktree: t.TempDir(), SessionName: "nomarker",
		FinalReport: report, Marker: "", // deliberately unset
		MaxRounds: 1, TimeoutS: 20, PiBin: pi, BackoffScale: 0.02,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := newTestSupervisor(t)
	if err := s.Start("nomarker"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, func() bool {
		st, _ := s.Status("nomarker")
		return st.(job.Status).State == "fatal" // round cap, not done
	})
	st, _ := s.Status("nomarker")
	if got := st.(job.Status).State; got != "fatal" {
		t.Fatalf("state = %q, want fatal (an empty marker must not finish a run)", got)
	}
}

// Stop is idempotent while a round is live: the second call reports
// "not running" instead of double-closing the stop channel.
func TestStopTwiceIsSafe(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_HANG"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "stoppable", Brief: brief, Worktree: t.TempDir(), SessionName: "stoppable",
		MaxRounds: 5, TimeoutS: 60, PiBin: pi, BackoffScale: 0.02,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	if err := s.Start("stoppable"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, func() bool { return s.RunningCount() == 1 })
	if err := s.Stop("stoppable"); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := s.Stop("stoppable"); err == nil {
		t.Fatal("second Stop must report not running, not panic on a closed channel")
	}
	waitFor(t, 30*time.Second, func() bool {
		st, _ := s.Status("stoppable")
		return st.(job.Status).State == "stopped"
	})
	if s.RunningCount() != 0 {
		t.Fatal("RunningCount must drop to 0 after a stop")
	}
	// State survives on disk so a later start resumes rather than restarts.
	st, err := job.LoadState("stoppable")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "stopped" || st.Round < 1 {
		t.Fatalf("persisted state = %+v", st)
	}
}

// Starting a job twice is refused: two round loops would fight over one
// session (invariant 1).
func TestStartTwiceRefused(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_HANG"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "dup", Brief: brief, Worktree: t.TempDir(), SessionName: "dup",
		MaxRounds: 3, TimeoutS: 60, PiBin: pi, BackoffScale: 0.02,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	if err := s.Start("dup"); err != nil {
		t.Fatal(err)
	}
	if err := s.Start("dup"); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second Start = %v, want a refusal", err)
	}
}

// Hot reload while a round loop is live must not race on the runner config
// (the review fixed three unlocked r.job reads). -race is the assertion.
func TestReloadWhileRunningIsRaceFree(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=2"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "hot", Brief: brief, Worktree: t.TempDir(), SessionName: "hot",
		MaxRounds: 3, TimeoutS: 30, PiBin: pi, BackoffScale: 0.02,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := newTestSupervisor(t)
	if err := s.Start("hot"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // config churn
		defer wg.Done()
		for range 20 {
			hot := j
			hot.Marker = "MARKER_" + time.Now().Format("150405.000000")
			if err := job.Save(hot); err != nil {
				return
			}
			_ = s.Reload()
			time.Sleep(5 * time.Millisecond)
		}
	}()
	go func() { // observer churn
		defer wg.Done()
		for range 50 {
			_ = s.StatusAll()
			_ = s.RunningCount()
			time.Sleep(5 * time.Millisecond)
		}
	}()
	wg.Wait()
	if err := s.Stop("hot"); err != nil {
		t.Fatal(err)
	}
}

// A job file removed from the jobs dir disappears from the next reload;
// a corrupt file is skipped without taking the daemon down.
func TestReloadDropsAndSkipsJobs(t *testing.T) {
	testEnv(t)
	writeJob(t, job.Job{Name: "keep", Brief: "/tmp/x", Worktree: t.TempDir(), MaxRounds: 1})
	writeJob(t, job.Job{Name: "gone", Brief: "/tmp/x", Worktree: t.TempDir(), MaxRounds: 1})
	s := newTestSupervisor(t)
	if len(s.StatusAll().([]job.Status)) != 2 {
		t.Fatal("expected two jobs")
	}
	if err := os.Remove(filepath.Join(job.JobsDir(), "gone.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job.JobsDir(), "broken.json"), []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("a corrupt job file must not fail the reload: %v", err)
	}
	all := s.StatusAll().([]job.Status)
	if len(all) != 1 || all[0].Name != "keep" {
		t.Fatalf("after reload = %#v, want only keep", all)
	}
	// A job whose persisted state says "running" is adopted as "stopped"
	// (resumable, never auto-running) when the runner is first created.
	if err := job.SaveState("keep", job.State{State: "running", Round: 2}); err != nil {
		t.Fatal(err)
	}
	fresh := New()
	if err := fresh.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	st, _ := fresh.Status("keep")
	if got := st.(job.Status); got.State != "stopped" || got.Round != 2 {
		t.Fatalf("adopted state = %+v, want stopped at round 2", got)
	}
	// A reload of an EXISTING runner keeps the live state: reload is for
	// config, not for re-reading runtime state (a round in flight must not
	// have its counter rewound to whatever is on disk).
	if err := job.SaveState("keep", job.State{State: "running", Round: 9}); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Status("keep")
	if got := st.(job.Status); got.Round != 0 {
		t.Fatalf("reload re-read runtime state: round = %d, want the live 0", got.Round)
	}
}

// LoadJobs on a missing jobs dir is an error the daemon logs and survives.
func TestLoadJobsMissingDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := New()
	if err := s.LoadJobs(); err == nil {
		t.Fatal("missing jobs dir must report an error")
	}
}

// Monitor refreshes the per-job status JSON on its tick, and Shutdown returns
// after the loops are down.
func TestMonitorWritesStatusFileAndShutdown(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_HANG"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "mon", Brief: brief, Worktree: t.TempDir(), SessionName: "mon",
		MaxRounds: 3, TimeoutS: 60, PiBin: pi, BackoffScale: 0.02,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	go s.Monitor(stop)
	t.Cleanup(func() { close(stop) })

	if err := s.Start("mon"); err != nil {
		t.Fatal(err)
	}
	_, _, _, statusPath := job.Paths("mon")
	waitFor(t, 30*time.Second, func() bool {
		data, err := os.ReadFile(statusPath)
		return err == nil && strings.Contains(string(data), `"name": "mon"`)
	})
	data, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	var st job.Status
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("status file is not valid JSON: %v", err)
	}
	if st.Name != "mon" || st.State != "running" {
		t.Fatalf("status file = %+v", st)
	}

	// Shutdown closes the supervisor's own stop channel and joins the loops.
	done := make(chan struct{})
	go func() { s.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Shutdown hung")
	}
	if s.RunningCount() != 0 {
		t.Fatal("jobs still active after Shutdown")
	}
}

// The CI-stall watcher below the cap only observes: no abort frame is written
// and the round keeps running (ADR-0004).
func TestCIStallBelowCapLeavesControlFileAlone(t *testing.T) {
	testEnv(t)
	sess := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(sess, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	watchStop, watchExited := make(chan struct{}), make(chan struct{})
	r := &runner{
		job: job.Job{Name: "obs", CIStallCap: 5, CIStallIdleS: 1, Worktree: t.TempDir()},
	}
	stopCh := make(chan struct{})
	go func() {
		defer close(watchExited)
		New().watchCIStalls(r, sess, 1, watchStop, stopCh)
	}()
	time.Sleep(300 * time.Millisecond)
	f, err := os.OpenFile(sess, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"content":"gh pr checks — waiting for CI"}` + "\n")
	_ = f.Close()

	// A stall fires, but with cap 5 nothing is interrupted and no frame lands.
	waitFor(t, 10*time.Second, func() bool { return r.stateSnapshot().CIStalls >= 1 })
	time.Sleep(300 * time.Millisecond)
	if data, err := os.ReadFile(job.Ctrl("obs")); err == nil && strings.Contains(string(data), `"abort"`) {
		t.Fatalf("below the cap an abort frame was written: %q", string(data))
	}
	if got := r.stateSnapshot(); got.State == "fatal" {
		t.Fatal("job went fatal below the CI-stall cap")
	}
	close(watchStop)
	<-watchExited
	// Closing the supervisor stop channel also stops the watcher.
	watchStop2, exited2 := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited2)
		New().watchCIStalls(r, sess, 2, watchStop2, stopCh)
	}()
	close(stopCh)
	select {
	case <-exited2:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher ignored the supervisor stop channel")
	}
}
