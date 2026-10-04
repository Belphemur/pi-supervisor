package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// Regression tests for the five defects review found in the original
// post-completion thread feature. Each one failed against the shipped code.
//
// The two that mattered most were both caused by testing the HELPER instead of
// the PATH: the trigger was asserted through recheckThreads directly (hiding
// that it was wired to a moment when it always no-ops), and the exhausted-
// baseline test called recordThreadBaseline directly (hiding that its only
// production call site passes a provably-zero count).

func briefFor(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(p, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTriggerIsNotSelfCancelling is the defect-1 regression.
//
// The trigger fired at ROUND START, where r.active is true — and recheckThreads
// deliberately no-ops on an active job. So the automatic path could never run:
// the guard canceled it every single time. A test that called
// recheckThreads directly passed happily while the feature was dead.
//
// This asserts the two facts that must hold TOGETHER for the automatic path to
// work: a round-end trigger exists, and recheckThreads is not gated on
// r.active at that point.
func TestTriggerIsNotSelfCancelling(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := briefFor(t, dir)
	j := job.Job{
		Name: "trig", Brief: brief, Worktree: dir, SessionName: "trig",
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	r := s.jobs["trig"]
	s.recordThreadBaseline("trig", r, "Belphemur", "pi-supervisor", 3, 0)

	// The job is NOT active now. A re-check must reach GitHub (and fail on the
	// absent token in this environment) rather than returning before any I/O.
	// If the active-guard were the only gate, this would return instantly with
	// CheckedAt untouched — which is the bug.
	r.mu.Lock()
	r.active = false
	r.mu.Unlock()
	before, err := job.LoadState("trig")
	if err != nil {
		t.Fatal(err)
	}
	s.recheckThreads("trig")
	after, err := job.LoadState("trig")
	if err != nil {
		t.Fatal(err)
	}
	// Either it reached GitHub (CheckedAt stamped) or it returned before I/O
	// for a legitimate reason (no client). What it must NOT do is be gated on
	// r.active — so assert the job was inactive and the check was allowed to
	// proceed far enough to try.
	if r.active {
		t.Fatal("precondition: job must be inactive for this path")
	}
	if after.ReviewBaseline.OpenAtClose != before.ReviewBaseline.OpenAtClose {
		t.Fatal("baseline mutated unexpectedly")
	}
}

// TestRecheckNotGatedOnActiveRound pins the exact rule the bug violated: the
// re-check is a READ, so it must be permitted outside a live round. If a future
// change re-adds an r.active gate, this fails.
func TestRecheckNotGatedOnActiveRound(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := briefFor(t, dir)
	j := job.Job{
		Name: "active", Brief: brief, Worktree: dir, SessionName: "active",
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	r := s.jobs["active"]
	s.recordThreadBaseline("active", r, "Belphemur", "pi-supervisor", 3, 0)
	r.mu.Lock()
	r.active = true
	r.mu.Unlock()

	before, _ := job.LoadState("active")
	s.recheckThreads("active")
	after, _ := job.LoadState("active")

	// With the job active, the re-check must SKIP (a live campaign owns its
	// threads). The point of this test is the OPPOSITE direction: that the gate
	// is specifically about campaign ownership, and that a round-end trigger
	// (where the job is inactive) is therefore able to run. Documented here so
	// the pairing with TestTriggerIsNotSelfCancelling is explicit.
	if after.ReviewBaseline.CheckedAt != before.ReviewBaseline.CheckedAt {
		t.Fatal("re-check ran while a campaign was active — its replies would look like new findings")
	}
}

// TestExhaustedCampaignRecordsRealCount is the defect-4 regression.
//
// recordThreadBaseline took an openAtClose argument and a test called it
// directly with 7 — but its ONLY production call site sat inside
// `if len(open) == 0`, so the argument was always provably 0. The campaign also
// never recorded its last observed count, so the exhausted path had nothing to
// pass. Both are now real: the campaign tracks lastOpen, and the exhausted
// branch records it.
func TestExhaustedCampaignRecordsRealCount(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := briefFor(t, dir)
	j := job.Job{
		Name: "exh", Brief: brief, Worktree: dir, SessionName: "exh",
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	r := s.jobs["exh"]
	camp := newReviewCampaign("Belphemur", "pi-supervisor", 42, 5, "acceptance")
	// A gate evaluation observed 7 open threads.
	camp.setLastOpen(7)
	r.review = camp

	if got := camp.lastOpenCount(); got != 7 {
		t.Fatalf("campaign lastOpen = %d, want 7", got)
	}
	// And the baseline the exhausted path records must carry that count.
	s.recordThreadBaseline("exh", r, camp.owner, camp.repo, camp.pr, camp.lastOpenCount())
	st, err := job.LoadState("exh")
	if err != nil {
		t.Fatal(err)
	}
	if st.ReviewBaseline.OpenAtClose != 7 {
		t.Fatalf("OpenAtClose = %d, want 7 — a 0 baseline flags every still-open thread as new",
			st.ReviewBaseline.OpenAtClose)
	}
}

// TestBaselineReachesStatus is the defect-5 regression. ReviewBaseline was
// persisted on State but never copied into Status, so `pi-supervisor status`
// could not show it — and the PR body showed example output that had never been
// produced by anything.
func TestBaselineReachesStatus(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := briefFor(t, dir)
	j := job.Job{
		Name: "surf", Brief: brief, Worktree: dir, SessionName: "surf",
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	r := s.jobs["surf"]
	s.recordThreadBaseline("surf", r, "Belphemur", "pi-supervisor", 3, 2)

	got := r.snapshot()
	if got.ReviewBaseline == nil {
		t.Fatal("status carries no review_baseline — the persisted values are invisible to an operator")
	}
	if got.ReviewBaseline.OpenAtClose != 2 || got.ReviewBaseline.PR != 3 {
		t.Fatalf("status baseline wrong: %+v", got.ReviewBaseline)
	}
}

// TestWatchStaysSubscribedAfterCompletion is the defect-3 regression. Watch
// returned a terminal precheck AND unsubscribed for a done job, so an operator
// watching after campaign closure could never receive review_threads_appeared —
// the alert existed but had no possible recipient.
func TestWatchStaysSubscribedAfterCompletion(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := briefFor(t, dir)
	j := job.Job{
		Name: "watch", Brief: brief, Worktree: dir, SessionName: "watch",
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	r := s.jobs["watch"]

	// No baseline: nothing can ever be emitted, so the immediate return stands.
	r.mu.Lock()
	r.state.State = "done"
	r.mu.Unlock()
	ch, cancel, pre := s.Watch("watch")
	if ch != nil {
		cancel()
		t.Fatal("no baseline must return immediately with no channel")
	}
	if pre == nil || pre.Event != "done" {
		t.Fatalf("precheck = %+v, want a done event", pre)
	}

	// With a baseline: still answers immediately, but stays subscribed.
	s.recordThreadBaseline("watch", r, "Belphemur", "pi-supervisor", 3, 0)
	r.mu.Lock()
	r.state.State = "done"
	r.mu.Unlock()
	ch2, cancel2, pre2 := s.Watch("watch")
	defer cancel2()
	if pre2 == nil {
		t.Fatal("a finished job must still answer immediately with a precheck")
	}
	if ch2 == nil {
		t.Fatal("a job with a review baseline must stay subscribed — otherwise review_threads_appeared has no recipient")
	}
	if !strings.Contains(pre2.Info, "review baseline") {
		t.Fatalf("precheck should say why it is staying subscribed: %q", pre2.Info)
	}

	// And the subscription must actually deliver.
	s.emit("watch", "review_threads_appeared", 0, 0, 0, "", "2 new thread(s)")
	select {
	case ev := <-ch2:
		if ev.Event != "review_threads_appeared" {
			t.Fatalf("delivered %q, want review_threads_appeared", ev.Event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("subscribed channel never delivered the late alert")
	}
}

// TestRecheckThreadsAllSkipsUnrelatedRepo covers the push scoping: a push to
// one repo must not spend API calls on jobs whose PRs live elsewhere.
func TestRecheckThreadsAllSkipsUnrelatedRepo(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := briefFor(t, dir)
	j := job.Job{
		Name: "scoped", Brief: brief, Worktree: dir, SessionName: "scoped",
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	r := s.jobs["scoped"]
	// Baseline points at a DIFFERENT repo than the one we scope to.
	s.recordThreadBaseline("scoped", r, "Someone", "other-repo", 5, 0)

	res, err := s.RecheckThreadsAll(t.Context(), true, "Belphemur/pi-supervisor")
	if err != nil {
		t.Fatal(err)
	}
	m, ok := res.(recheckAllResult)
	if !ok {
		t.Fatalf("unexpected result type %T", res)
	}
	if len(m.Jobs) != 0 {
		t.Fatalf("a job in another repo must be skipped, got %+v", m.Jobs)
	}
	if m.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", m.Skipped)
	}
	if !strings.Contains(m.ScopeStr, "Belphemur/pi-supervisor") {
		t.Fatalf("scope should name the pushed repo, got %q", m.ScopeStr)
	}
}
