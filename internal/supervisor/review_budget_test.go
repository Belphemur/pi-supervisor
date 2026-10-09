package supervisor

// The campaign budget is spent on WORK, not on loop rounds: a provider that
// answers with an empty 0-token response (observed live on flambette#65's
// campaign, rounds 5-6: 3-4s each, runlog 0 bytes, content []) produced
// nothing and must not consume an operator round. Exhaustion still arrives
// through the raw-round backstop (3x budget), so a permanently broken
// provider cannot spin forever either.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pi-supervisor/internal/job"
	"pi-supervisor/internal/review"
)

func TestEmptyReviewRoundsDoNotSpendBudget(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "final.md")
	if err := os.WriteFile(report, []byte("build report"), 0o644); err != nil {
		t.Fatal(err)
	}
	rb := filepath.Join(dir, "review_brief.md")
	if err := os.WriteFile(rb, []byte("TEST_EMPTY"), 0o644); err != nil {
		t.Fatal(err)
	}

	j := job.Job{
		Name: "rbudget", Brief: brief, Worktree: dir, SessionName: "rbudget",
		Marker: "RSESSION_RBUDGET_DONE", FinalReport: report,
		MaxRounds: 6, TimeoutS: 60, PiBin: pi, BackoffScale: 0.02,
		ReviewBrief: rb,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	doneShape(t, s, j.Name)
	installGateReader(t, &fakeGateReader{
		threads: []review.Thread{{ThreadID: "PRRT_test1"}},
		roll:    &review.CIRollup{Verdict: "pass"},
	})

	// Budget 1: with the OLD raw-round cap the FIRST empty round (round
	// count 1) would exhaust immediately. With spent-accounting the empty
	// rounds are free; the campaign only terminates on the 3x raw backstop
	// — so at least TWO empty review rounds must run before exhaustion.
	// Subscribe AFTER StartReview: on a done job a pre-arm Watch returns
	// the precheck immediately and never registers (ADR-0003).
	if _, err := s.StartReview(context.Background(), j.Name, 54, 1, "acceptance"); err != nil {
		t.Fatalf("StartReview: %v", err)
	}
	ch, cancel, _ := s.Watch(j.Name)
	defer cancel()

	rounds := 0
	deadline := time.After(60 * time.Second)
	for {
		select {
		case e := <-ch:
			switch e.Event {
			case "review_round_done":
				rounds++
			case "review_exhausted", "fatal", "review_done":
				if rounds < 2 {
					t.Fatalf("campaign ended after %d review round(s) on a 1-round budget — empty rounds consumed the budget", rounds)
				}
				// The job exhausted via the backstop with threads still open.
				st, err := job.LoadState(j.Name)
				if err != nil {
					t.Fatal(err)
				}
				if st.State != "fatal" {
					t.Errorf("state = %q, want fatal (review_exhausted)", st.State)
				}
				return
			}
		case <-deadline:
			t.Fatal("campaign never reached a terminal verdict")
		}
	}
}
