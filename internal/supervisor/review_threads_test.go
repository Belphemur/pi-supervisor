package supervisor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// briefFile writes a throwaway brief and returns its path.
func briefFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(p, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The live gap this closes (PR #3): a review campaign closed clean at 17:26,
// and bot findings landed at 17:33 and 17:53. Every review verb requires a live
// round and the campaign stanza is consumed one-shot, so those findings had no
// answering round — they were only found because a human went looking.
//
// The baseline is what makes them detectable afterwards without polling.

// TestBaselineRecordedOnStateNotCampaign pins WHERE the baseline lives. It must
// be on job.State, not the in-memory reviewCampaign: the campaign object is
// gone once the job finishes, which is exactly the moment the baseline is
// needed.
func TestBaselineRecordedOnStateNotCampaign(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := briefFile(t, dir)
	j := job.Job{
		Name: "bl", Brief: brief, Worktree: dir, SessionName: "bl",
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	r := s.jobs["bl"]
	if r == nil {
		t.Fatal("job not loaded")
	}
	// Arm a campaign so the recorder has something to write against.
	r.review = newReviewCampaign("Belphemur", "pi-supervisor", 4242, 5, "acceptance")
	s.recordThreadBaseline("bl", r, "Belphemur", "pi-supervisor", 4242, 3)

	st, err := job.LoadState("bl")
	if err != nil {
		t.Fatal(err)
	}
	if st.ReviewBaseline == nil {
		t.Fatal("baseline not persisted to state — it cannot outlive the campaign this way")
	}
	bl := st.ReviewBaseline
	if bl.OpenAtClose != 3 {
		t.Fatalf("OpenAtClose = %d, want 3", bl.OpenAtClose)
	}
	if bl.PR != 4242 || bl.Owner != "Belphemur" || bl.Repo != "pi-supervisor" {
		t.Fatalf("baseline target wrong: %+v", bl)
	}
	if bl.ClosedAt == "" {
		t.Fatal("ClosedAt not stamped")
	}
	// OpenNow is seeded from the baseline so "new since close" reads 0 rather
	// than being unknown.
	if bl.OpenNow != 3 {
		t.Fatalf("OpenNow = %d, want 3 (seeded from baseline)", bl.OpenNow)
	}
}

// TestBaselineRecordsNonZeroForExhausted guards a subtle bug: a campaign that
// ends on review_exhausted has threads STILL open. Hardcoding 0 there would
// record a baseline that makes the next re-check report every open thread as
// brand new.
func TestBaselineRecordsNonZeroForExhausted(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := briefFile(t, dir)
	j := job.Job{
		Name: "blx", Brief: brief, Worktree: dir, SessionName: "blx",
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	r := s.jobs["blx"]
	// 7 threads still open at the cap.
	s.recordThreadBaseline("blx", r, "Belphemur", "pi-supervisor", 7, 7)

	st, err := job.LoadState("blx")
	if err != nil {
		t.Fatal(err)
	}
	if st.ReviewBaseline.OpenAtClose != 7 {
		t.Fatalf("OpenAtClose = %d, want 7 — a 0 here would flag 7 threads as new",
			st.ReviewBaseline.OpenAtClose)
	}
}

// TestRecheckNoOpWithoutBaseline is the cost guarantee: a build-only job that
// never reviewed anything must do ZERO GitHub calls. This is what keeps the
// feature from degrading into the polling loop ADR-0012 forbids.
func TestRecheckNoOpWithoutBaseline(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := briefFile(t, dir)
	j := job.Job{
		Name: "nobl", Brief: brief, Worktree: dir, SessionName: "nobl",
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	r := s.jobs["nobl"]
	if r.state.ReviewBaseline != nil {
		t.Fatal("precondition: no baseline")
	}

	// No GitHub token in this environment, so if the re-check reached the API
	// it would log a failure. Reaching here without one is the proof it
	// returned before any I/O.
	done := make(chan struct{})
	go func() { s.recheckThreads("nobl"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recheckThreads blocked — it must be a fast no-op with no baseline")
	}

	if r.state.ReviewBaseline != nil {
		t.Fatal("re-check invented a baseline on a job that never reviewed")
	}
}

// TestRecheckSkipsActiveCampaign: a re-check mid-campaign would report the
// campaign's own in-flight replies as new findings from nowhere.
func TestRecheckSkipsActiveCampaign(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := briefFile(t, dir)
	j := job.Job{
		Name: "act", Brief: brief, Worktree: dir, SessionName: "act",
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	r := s.jobs["act"]
	s.recordThreadBaseline("act", r, "Belphemur", "pi-supervisor", 9, 4)
	r.mu.Lock()
	r.active = true
	r.mu.Unlock()

	before, err := job.LoadState("act")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.recheckThreads("act"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recheckThreads blocked during an active campaign")
	}

	after, err := job.LoadState("act")
	if err != nil {
		t.Fatal(err)
	}
	// CheckedAt must be untouched: an active campaign owns the threads.
	if after.ReviewBaseline.CheckedAt != before.ReviewBaseline.CheckedAt {
		t.Fatalf("re-check ran during an active campaign: CheckedAt %q -> %q",
			before.ReviewBaseline.CheckedAt, after.ReviewBaseline.CheckedAt)
	}
}

// TestShortSHA documents the display helper; "" must render as (unknown), not
// as an empty string that would produce a confusing "moved  -> abc1234".
func TestShortSHA(t *testing.T) {
	cases := map[string]string{
		"":                     "(unknown)",
		"abc":                  "abc",
		"0123456789abcdef":     "0123456",
		"0123456789abcdef0123": "0123456",
	}
	for in, want := range cases {
		if got := shortSHA(in); got != want {
			t.Errorf("shortSHA(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHeadSHADegradesGracefully: headSHA is only a change signal, so an
// unreadable worktree must yield "" rather than failing a re-check.
func TestHeadSHADegradesGracefully(t *testing.T) {
	testEnv(t)
	j := job.Job{Name: "h", Worktree: "/definitely/not/a/repo"}
	writeJob(t, j)
	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	if got := s.headSHA(s.jobs["h"]); got != "" {
		t.Fatalf("headSHA on a non-repo = %q, want empty", got)
	}
}
