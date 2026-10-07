package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pi-supervisor/internal/fault"
	"pi-supervisor/internal/job"
	"pi-supervisor/internal/review"
)

// ADR-0016: the manual review re-entry. Three defects made re-running a
// review campaign on a DONE job impossible without a hand dance (delete the
// state file, restart the daemon, rewrite the marker, arm, start):
//
//  1. Start refused a done job even when a campaign was armed, forcing the
//     state-file deletion.
//  2. Start set state "running" even with a campaign armed, so the loop's
//     `reviewing` flag stayed false: the campaign's budget never applied and
//     its gate (reviewGate) never ran — the campaign could not terminate.
//  3. The marker gate had no live-campaign guard: the sticky MarkerSeen latch
//     of the PREVIOUS campaign plus the cumulative transcript scan closed the
//     resumed job done at the end of round 1 (live on mealime-rebase54).
//
// These tests drive the REAL loop with the fake pi, in the exact shape the
// operator hit.

// fakeGateReader is a reviewReader with canned verdicts.
type fakeGateReader struct {
	threads []review.Thread
	roll    *review.CIRollup
}

func (f *fakeGateReader) ListThreads(context.Context, string, string, int) ([]review.Thread, error) {
	return f.threads, nil
}

func (f *fakeGateReader) CheckCI(context.Context, string, string, int) (*review.CIRollup, error) {
	return f.roll, nil
}

func installGateReader(t *testing.T, fr *fakeGateReader) {
	t.Helper()
	prev := gateReader
	gateReader = func(*Supervisor, context.Context, string, string) (reviewReader, error) {
		return fr, nil
	}
	t.Cleanup(func() { gateReader = prev })
}

// doneJobShape builds a job whose build phase is finished: state done, sticky
// marker latch, marker in the transcript (byte 0, as a previous campaign
// leaves it), final report present, and a linked pr_url.
func doneJobShape(t *testing.T, s *Supervisor, name string) {
	t.Helper()
	r := s.jobs[name]
	if r == nil {
		t.Fatal("runner missing")
	}
	r.mu.Lock()
	r.state.State = "done"
	r.state.MarkerSeen = true
	r.state.PRURL = "https://github.com/Belphemur/flambette/pull/54"
	r.mu.Unlock()
}

// TestReviewReentryDoneJobRunsCampaign is the headline regression: a done job
// (marker latched, report present) + an armed campaign must start into
// `reviewing`, MUST NOT classify done from the old marker, and must end on
// the campaign's own verdict (review_done) — with the campaign's budget
// governing (job MaxRounds 4 > campaign rounds 1).
func TestReviewReentryDoneJobRunsCampaign(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "final.md")
	if err := os.WriteFile(report, []byte("campaign 1 report"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "reentry", Brief: brief, Worktree: dir, SessionName: "reentry",
		Marker: "REENTRY_DONE", FinalReport: report,
		MaxRounds: 4, TimeoutS: 60, PiBin: pi, BackoffScale: 0.02,
	}
	pinSession(t, &j, brief)
	// The previous campaign's marker, sitting in the transcript from byte 0.
	sess := j.SessionPath
	f, err := os.OpenFile(sess, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(assistantRecord("all work finished — REENTRY_DONE") + "\n")
	_ = f.Close()
	writeJob(t, j)

	installGateReader(t, &fakeGateReader{roll: &review.CIRollup{Verdict: "pass"}})

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	// Subscribe FIRST: a Watch issued after the done-state is set would take
	// the precheck path (nil channel) instead of a live subscription.
	ch, cancel, _ := s.Watch("reentry")
	defer cancel()
	doneShape(t, s, "reentry")

	if _, err := s.StartReview(context.Background(), "reentry", 54, 1, "acceptance"); err != nil {
		t.Fatalf("arming on a done job must work: %v", err)
	}
	// ADR-0016 owner correction: arming LAUNCHES the campaign — no separate
	// start. Assert the loop came up instead of calling Start by hand.
	if st := mustState(t, "reentry"); st.State != "reviewing" {
		t.Fatalf("state after StartReview = %q, want reviewing (arm must start)", st.State)
	}

	// The loop must end on the CAMPAIGN's verdict, never on the old marker.
	deadline := time.After(60 * time.Second)
	sawReviewingState, sawReviewDone := false, false
	for !sawReviewDone {
		select {
		case e := <-ch:
			switch e.Event {
			case "reviewing":
				sawReviewingState = true
			case "done":
				t.Fatal("the marker gate classified a live campaign done — " +
					"the previous campaign's marker leaked through (ADR-0016 defect 3)")
			case "review_done":
				sawReviewDone = true
			case "fatal":
				t.Fatalf("campaign ended fatal instead of its own gate: %s", e.Info)
			}
		case <-deadline:
			t.Fatal("no review_done within 60s")
		}
	}
	if !sawReviewingState {
		t.Fatal("the job never emitted `reviewing` — the campaign never owned the loop")
	}
	st, _ := s.Status("reentry")
	m, _ := st.(job.Status)
	if m.State != "done" {
		t.Fatalf("state = %q, want done after review_done", m.State)
	}
	if m.Review != nil {
		t.Fatal("campaign still active after review_done — a spent campaign must be cleared")
	}
	// The spent campaign must not let a later start re-enter reviewing.
	if err := s.Start("reentry"); err == nil {
		t.Fatal("start after review_done must be refused (campaign spent)")
	}
}

// Counterfactual for defect 1: without an armed campaign the done refusal is
// unchanged — `done` still means "clear state to rerun".
func TestStartStillRefusesDoneWithoutCampaign(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "donerefusal", Brief: brief, Worktree: dir, SessionName: "donerefusal",
		Marker: "X_DONE", MaxRounds: 1, TimeoutS: 30, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	doneShape(t, s, "donerefusal")

	err := s.Start("donerefusal")
	if err == nil {
		t.Fatal("start on a done job without a campaign must be refused")
	}
	if fault.KindOf(err) != fault.KindAlreadyDone {
		t.Fatalf("refusal must carry KindAlreadyDone, got: %v (%v)", fault.KindOf(err), err)
	}
}

// Defect 2 in isolation: a campaign armed on a STOPPED job must make start
// enter `reviewing` (not `running`), so the campaign budget and gate govern.
func TestStartWithCampaignEntersReviewing(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "enterreview", Brief: brief, Worktree: dir, SessionName: "enterreview",
		Marker: "Y_DONE", MaxRounds: 2, TimeoutS: 30, PiBin: pi, BackoffScale: 0.02,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	installGateReader(t, &fakeGateReader{roll: &review.CIRollup{Verdict: "pass"}})

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	r := s.jobs["enterreview"]
	r.mu.Lock()
	r.state.PRURL = "https://github.com/Belphemur/flambette/pull/54"
	r.mu.Unlock()

	// Subscribe BEFORE arming: StartReview now LAUNCHES the campaign
	// (ADR-0016 owner correction), so the reviewing event fires inside it —
	// a subscription after the arm would miss it.
	ch, cancel, _ := s.Watch("enterreview")
	defer cancel()
	if _, err := s.StartReview(context.Background(), "enterreview", 54, 1, "acceptance"); err != nil {
		t.Fatalf("arming: %v", err)
	}
	// Arming launched the loop; no separate Start exists any more.

	// The first event after job_started must be the campaign's, and the run
	// must end review_done (0 threads + CI pass) — not run as a build job.
	deadline := time.After(60 * time.Second)
	sawReviewingState := false
	for {
		select {
		case e := <-ch:
			switch e.Event {
			case "reviewing":
				sawReviewingState = true
			case "review_done":
				if !sawReviewingState {
					t.Fatal("review_done without a preceding `reviewing` event — start did not enter reviewing")
				}
				return
			case "done":
				t.Fatal("campaign closed as plain `done` — the loop never entered reviewing")
			case "fatal":
				t.Fatalf("fatal: %s", e.Info)
			}
		case <-deadline:
			t.Fatal("no review_done within 60s")
		}
	}
}

// finish() must spend the campaign: any terminal state clears it, so a later
// start cannot re-enter reviewing with a stale budget. Operator stops do NOT
// finish — they must keep the campaign armed for resume.
func TestFinishSpendsCampaignButStopDoesNot(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "spendcamp", Brief: brief, Worktree: dir, SessionName: "spendcamp",
		Marker: "Z_DONE", MaxRounds: 2, TimeoutS: 30, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	r := s.jobs["spendcamp"]
	r.mu.Lock()
	r.state.PRURL = "https://github.com/Belphemur/flambette/pull/54"
	r.mu.Unlock()
	if _, err := s.StartReview(context.Background(), "spendcamp", 54, 2, "acceptance"); err != nil {
		t.Fatalf("arming: %v", err)
	}

	r.mu.Lock()
	if r.review == nil {
		r.mu.Unlock()
		t.Fatal("campaign not armed")
	}
	r.mu.Unlock()

	// Operator stop: the campaign must SURVIVE (resume semantics). Stop
	// requires an active runner, so activate it the way a running loop would.
	r.mu.Lock()
	r.active = true
	r.stopCh = make(chan struct{})
	r.mu.Unlock()
	if err := s.Stop("spendcamp"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	r.mu.Lock()
	if r.review == nil {
		r.mu.Unlock()
		t.Fatal("Stop must not spend the campaign — start must be able to resume it")
	}
	r.mu.Unlock()

	// finish(): any terminal state spends it.
	r.finish("fatal", "test")
	r.mu.Lock()
	spent := r.review == nil
	r.mu.Unlock()
	if !spent {
		t.Fatal("finish must clear the campaign")
	}
}

// doneShape marks a freshly loaded runner as a finished build job (state,
// latch, pr_url). Used instead of running a real campaign to reach done.
func doneShape(t *testing.T, s *Supervisor, name string) {
	t.Helper()
	doneJobShape(t, s, name)
}
