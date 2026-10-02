package supervisor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// A live watch subscriber must receive the job's events while the round loop
// runs, and the loop must not be slowed by it.
func TestWatchReceivesRoundEvents(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=3"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "watched", Brief: brief, Worktree: t.TempDir(), SessionName: "watched",
		MaxRounds: 2, TimeoutS: 30, PiBin: pi, BackoffScale: 0.02,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := newTestSupervisor(t)
	ch, cancel, pre := s.Watch("watched")
	if pre != nil {
		t.Fatalf("precheck on a fresh job: %+v", *pre)
	}
	defer cancel()

	if err := s.Start("watched"); err != nil {
		t.Fatal(err)
	}

	// Expect at least job_started then round_done within the round budget.
	want := map[string]bool{"job_started": false, "round_done": false}
	deadline := time.After(45 * time.Second)
	for remaining := 2; remaining > 0; {
		select {
		case e := <-ch:
			if e.Job != "watched" {
				continue
			}
			if _, ok := want[e.Event]; ok && !want[e.Event] {
				want[e.Event] = true
				remaining--
			}
		case <-deadline:
			t.Fatalf("missing events after 45s: got %v", want)
		}
	}
}

// Watching an already-terminal job returns the precheck immediately: the run
// is over, the watch must not block (ADR-0003 requirement 4).
func TestWatchOnFinishedJobReturnsImmediately(t *testing.T) {
	testEnv(t)
	writeJob(t, job.Job{Name: "over", Brief: "/tmp/x.md", Worktree: t.TempDir(),
		SessionName: "over", MaxRounds: 1, TimeoutS: 10, PiBin: "true"})
	if err := job.SaveState("over", job.State{Round: 1, State: "done", LastRC: 0, LastDurS: 5}); err != nil {
		t.Fatal(err)
	}

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, cancel, pre := s.Watch("over")
	defer cancel()
	if pre == nil {
		t.Fatal("expected a precheck event for a done job, got a live channel")
	}
	if pre.Event != "done" {
		t.Fatalf("precheck event = %q, want done", pre.Event)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("precheck took %s, want immediate", el)
	}
}

// An unknown job name must not be silently watchable forever — the control
// layer guards on Status, but Watch itself must tolerate the lookup miss.
func TestWatchUnknownJobSubscribesAnyway(t *testing.T) {
	testEnv(t)
	s := New()
	ch, cancel, pre := s.Watch("does-not-exist")
	defer cancel()
	if pre != nil {
		t.Fatalf("unexpected precheck: %+v", *pre)
	}
	if ch == nil {
		t.Fatal("expected a live channel for an unknown job (guard lives in control)")
	}
}

// Stopped is terminal for the watch: operator stop ends the run.
func TestWatchSeesStoppedOnOperatorStop(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=30"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "watchstop", Brief: brief, Worktree: t.TempDir(), SessionName: "watchstop",
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := newTestSupervisor(t)
	ch, cancel, _ := s.Watch("watchstop")
	defer cancel()
	if err := s.Start("watchstop"); err != nil {
		t.Fatal(err)
	}
	// Wait for the round to be live, then stop it.
	waitFor(t, 30*time.Second, func() bool { return s.RunningCount() == 1 })
	if err := s.Stop("watchstop"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(60 * time.Second)
	for {
		select {
		case e := <-ch:
			if e.Event == "stopped" {
				return // terminal: the watch would close here
			}
		case <-deadline:
			t.Fatal("no stopped event within 60s of operator stop")
		}
	}
}
