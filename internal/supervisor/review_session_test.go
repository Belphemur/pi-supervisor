package supervisor

// ADR-0018: a review campaign runs in its OWN session. Resuming the finished
// build session made every campaign round re-verify old work and exit — the
// model's own context said the work was complete (live: mealime-search3,
// flambette#58, five ~40s rounds, zero threads, review_exhausted).
//
// With review_brief on the job, arming clears the build pin and the
// campaign's round 1 LAUNCHes a fresh session seeded with the brief; every
// later campaign round resumes that session (invariant 1 within the
// campaign). Without review_brief, the legacy resume behavior stands and the
// daemon warns.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pi-supervisor/internal/job"
	"pi-supervisor/internal/journal"
	"pi-supervisor/internal/review"
)

// safeBuf is a mutex-guarded strings.Builder: journal handlers write from
// their own goroutines.
type safeBuf struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

// doneJobWithReview builds a done job (marker latched, pr_url linked, build
// session pinned) and returns it with the build session's path.
func doneJobWithReview(t *testing.T, name string, reviewBrief string) (job.Job, string) {
	t.Helper()
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
	j := job.Job{
		Name: name, Brief: brief, Worktree: dir, SessionName: name,
		Marker: "RSESSION_" + strings.ToUpper(name) + "_DONE", FinalReport: report,
		MaxRounds: 4, TimeoutS: 60, PiBin: pi, BackoffScale: 0.02,
	}
	if reviewBrief != "" {
		rb := filepath.Join(dir, "review_brief.md")
		// TEST_MKSESSION: fake-pi creates the round's transcript ONLY on this
		// variant — a campaign that adopted a fresh session therefore proves
		// the round LAUNCHed with THIS file as its prompt.
		if err := os.WriteFile(rb, []byte("TEST_MKSESSION"), 0o644); err != nil {
			t.Fatal(err)
		}
		j.ReviewBrief = rb
	}
	pinSession(t, &j, brief)
	buildSess := j.SessionPath
	// The finished build session carries the completion narrative.
	f, err := os.OpenFile(buildSess, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(assistantRecord("all work verified complete — "+j.Marker) + "\n")
	_ = f.Close()
	writeJob(t, j)
	return j, buildSess
}

// waitForCampaignEnd subscribes and waits for the running campaign's
// terminal verdict, returning the job's session path afterwards. It does NOT
// arm: StartReview already launched the campaign (one call does both).
func waitForCampaignEnd(t *testing.T, s *Supervisor, name string) string {
	t.Helper()
	ch, cancel, _ := s.Watch(name)
	defer cancel()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case e := <-ch:
			switch e.Event {
			case "review_done", "review_exhausted", "fatal":
				r := s.jobs[name]
				r.mu.Lock()
				p := r.job.SessionPath
				r.mu.Unlock()
				return p
			}
		case <-deadline:
			t.Fatal("campaign never reached a terminal verdict")
		}
	}
}

// With review_brief, arming clears the build pin immediately (and persists
// the clearing), the campaign LAUNCHes a fresh session seeded with the brief,
// and the build session file itself is left untouched on disk.
func TestCampaignArmedWithReviewBriefLaunchesFreshSession(t *testing.T) {
	testEnv(t)
	j, buildSess := doneJobWithReview(t, "rsession", "yes")

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	doneShape(t, s, j.Name)

	// The gate reader must be installed BEFORE arming: StartReview launches
	// the campaign, so the gate reads happen inside it.
	installGateReader(t, &fakeGateReader{roll: &review.CIRollup{Verdict: "pass"}})

	if _, err := s.StartReview(context.Background(), j.Name, 54, 1, "acceptance"); err != nil {
		t.Fatalf("StartReview: %v", err)
	}
	// The clear happens AT ARM TIME, synchronously — and is on disk.
	if got := mustStateJobSession(t, j.Name); got != "" {
		t.Fatalf("session pin after arm = %q, want cleared (ADR-0018)", got)
	}
	if _, err := os.Stat(buildSess); err != nil {
		t.Fatalf("build session file must survive the clear: %v", err)
	}

	fresh := waitForCampaignEnd(t, s, j.Name)
	if fresh == "" {
		t.Fatal("campaign ended without a session pin — capture failed")
	}
	if fresh == buildSess {
		t.Fatalf("campaign reused the build session %s — the ADR-0018 poison", buildSess)
	}
}

// The poison case, pinned as behavior: WITHOUT review_brief the campaign
// resumes the build session (legacy), and the operator is warned in the
// journal.
func TestCampaignArmedWithoutReviewBriefKeepsBuildSession(t *testing.T) {
	testEnv(t)
	j, buildSess := doneJobWithReview(t, "rlegacy", "")

	var logs safeBuf
	prev := journal.SetOutput(&logs)
	defer journal.SetOutput(prev)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	doneShape(t, s, j.Name)

	installGateReader(t, &fakeGateReader{roll: &review.CIRollup{Verdict: "pass"}})
	if _, err := s.StartReview(context.Background(), j.Name, 54, 1, "acceptance"); err != nil {
		t.Fatalf("StartReview: %v", err)
	}
	if got := mustStateJobSession(t, j.Name); got != buildSess {
		t.Fatalf("session pin after arm = %q, want the build session %q (legacy: no review_brief)", got, buildSess)
	}
	waitForCampaignEnd(t, s, j.Name)
	if !strings.Contains(logs.String(), "WITHOUT review_brief") {
		t.Fatalf("missing legacy-poison warning in journal: %q", logs.String())
	}
}

// mustStateJobSession reads the PERSISTED job file's session pin.
func mustStateJobSession(t *testing.T, name string) string {
	t.Helper()
	j, err := job.Load(filepath.Join(job.JobsDir(), name+".json"))
	if err != nil {
		t.Fatalf("job.Load(%s): %v", name, err)
	}
	return j.SessionPath
}
