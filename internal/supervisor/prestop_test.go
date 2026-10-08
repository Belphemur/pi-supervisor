package supervisor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// TestStopBeforeFirstRoundStaysStopped covers the Start()->loop() race:
// Stop() closed stopCh and applied the round floor (state.Round >= 1) BEFORE
// the loop goroutine's first step. The loop then computed round=2 > maxRounds
// and fired the round-cap fatal, overwriting the stopped state with fatal.
// A stop is terminal — the loop must classify nothing and return silently.
func TestStopBeforeFirstRoundStaysStopped(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=30"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "prestop", Brief: brief, Worktree: t.TempDir(), SessionName: "prestop",
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := newTestSupervisor(t)
	if err := s.Start("prestop"); err != nil {
		t.Fatal(err)
	}
	// Stop immediately — the round-floor lands before the loop goroutine's
	// first step, the exact window the bug lives in.
	if err := s.Stop("prestop"); err != nil {
		t.Fatal(err)
	}
	// Give the loop goroutine every chance to misclassify: with the bug it
	// fired the cap fatal within milliseconds of starting.
	time.Sleep(500 * time.Millisecond)

	st, err := job.LoadState("prestop")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "stopped" {
		t.Errorf("state = %q after an operator stop, want stopped (StopSource=%q)",
			st.State, st.StopSource)
	}
	if st.StopSource != "operator" {
		t.Errorf("StopSource = %q, want operator", st.StopSource)
	}
}
