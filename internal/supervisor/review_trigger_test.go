package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"pi-supervisor/internal/job"
)

// TestRecheckRunsWhenJobCloses is the trigger-placement regression.
//
// The re-check was fired at round start, then at round end — both inside the
// round, where r.active is still true. recheckThreads no-ops on an active job
// (a live campaign's own replies would be miscounted as new findings), so the
// automatic path NEVER performed a check. Two reviewers caught it separately.
//
// It must now fire from the completion gate, right after active goes false. This
// asserts the two facts that must hold together: a baseline exists, and the job
// is INACTIVE, so the guard cannot cancel the check.
func TestRecheckRunsWhenJobCloses(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "closed", Brief: brief, Worktree: dir, SessionName: "closed",
		Marker: "ALL_CLOSED_DONE", MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	r := s.jobs["closed"]
	s.recordThreadBaseline("closed", r, "Belphemur", "pi-supervisor", 3, 0)

	// Simulate what the gate does: active=false, then the trigger.
	r.mu.Lock()
	r.state.State, r.active = "done", false
	r.mu.Unlock()

	called := false
	prev := listOpenThreads
	listOpenThreads = func(*Supervisor, context.Context, string, string, int) (int, error) {
		called = true
		return 0, nil
	}
	t.Cleanup(func() { listOpenThreads = prev })

	s.recheckThreads("closed")
	if !called {
		t.Fatal("the re-check never reached GitHub — the active-guard canceled it, " +
			"so the trigger is still wired to a point where it cannot fire")
	}

	// And while active, it must still skip.
	r.mu.Lock()
	r.active = true
	r.mu.Unlock()
	called = false
	s.recheckThreads("closed")
	if called {
		t.Fatal("re-check ran during an active campaign")
	}
}
