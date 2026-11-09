package supervisor

import (
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// finish() is the terminal funnel: a state persisted through it must carry
// the run's end stamp (ADR-0025 §4.1) — it is the janitor's age anchor.
func TestFinishStampsEndedAt(t *testing.T) {
	testEnv(t)
	writeJob(t, job.Job{
		Name: "stamp", Brief: "/tmp/x.md", Worktree: t.TempDir(),
		SessionName: "stamp", MaxRounds: 1, TimeoutS: 5, PiBin: "true",
	})
	s := newTestSupervisor(t)
	s.mu.Lock()
	r := s.jobs["stamp"]
	s.mu.Unlock()
	if r == nil {
		t.Fatal("runner not registered")
	}
	r.finish("fatal", "test diag")
	st, err := job.LoadState("stamp")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "fatal" {
		t.Fatalf("state = %q, want fatal", st.State)
	}
	if st.EndedAt == "" {
		t.Fatal("terminal persistence must stamp EndedAt")
	}
	if _, err := time.Parse(time.RFC3339, st.EndedAt); err != nil {
		t.Fatalf("EndedAt %q is not RFC3339: %v", st.EndedAt, err)
	}
}

// A stop is NOT a terminal build state (ADR-0025 §2): neither Stop()'s own
// write nor the loop's idempotent second write may stamp EndedAt, because
// "stopped" is never a janitor candidate.
func TestStopDoesNotStampEndedAt(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)
	brief := "/tmp/x.md"
	j := job.Job{
		Name: "nos", Brief: brief, Worktree: t.TempDir(), SessionName: "nos",
		MaxRounds: 5, TimeoutS: 60, PiBin: pi, BackoffScale: 0.02,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := newTestSupervisor(t)
	if err := s.Start("nos"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, func() bool { return s.RunningCount() == 1 })
	if err := s.Stop("nos"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	var st job.State
	waitFor(t, 30*time.Second, func() bool {
		var err error
		if st, err = job.LoadState("nos"); err != nil {
			return false
		}
		return st.State == "stopped" && st.Round >= 1
	})
	if st.State != "stopped" || !strings.Contains(st.State, "stopped") {
		t.Fatalf("persisted state = %+v", st)
	}
	if st.EndedAt != "" {
		t.Fatalf("a stopped run must never stamp EndedAt, got %q", st.EndedAt)
	}
}
