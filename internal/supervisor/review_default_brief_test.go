package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-supervisor/internal/job"
)

// Owner directive 2026-10-10: triggering a review must never fall back to the
// build continuation. A job without review_brief gets a generated brief and a
// cleared build pin at arm time.
func TestFreshCampaignSessionGeneratesBriefWhenAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	s := New()
	wt := t.TempDir()
	j := job.Job{Name: "briefless", Worktree: wt, Brief: filepath.Join(wt, "b.md"), Cont: filepath.Join(wt, "c.txt"),
		SessionPath: filepath.Join(t.TempDir(), "build-session.jsonl")}
	if err := os.WriteFile(j.SessionPath, []byte(`{"x":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(job.JobsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := job.Save(j); err != nil {
		t.Fatal(err)
	}
	r := &runner{job: j}
	r.state.PRURL = "https://github.com/Belphemur/flambette/pull/68"

	s.freshCampaignSession(r, j.Name)

	r.mu.Lock()
	got := r.job.ReviewBrief
	pin := r.job.SessionPath
	r.mu.Unlock()
	if got == "" {
		t.Fatal("no review brief generated for a job without one")
	}
	if pin != "" {
		t.Fatalf("build pin not cleared: %q", pin)
	}
	body, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "list_threads") || !strings.Contains(string(body), "post_replies") {
		t.Fatalf("generated brief lacks the shim workflow:\n%s", body)
	}
	if !strings.Contains(got, "briefs") {
		t.Fatalf("brief written outside the briefs dir: %q", got)
	}
	// Persisted to the job def too.
	saved, err := job.Load(filepath.Join(job.JobsDir(), j.Name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if saved.ReviewBrief != got {
		t.Fatalf("job def not updated: %q", saved.ReviewBrief)
	}
}

// A job WITH a review_brief keeps the existing behavior exactly: pin cleared,
// the operator's brief used, nothing generated.
func TestFreshCampaignSessionKeepsOperatorBrief(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	s := New()
	wt := t.TempDir()
	brief := filepath.Join(wt, "review_brief.md")
	if err := os.WriteFile(brief, []byte("# operator brief"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{Name: "withbrief", Worktree: wt, Brief: filepath.Join(wt, "b.md"), ReviewBrief: brief,
		SessionPath: filepath.Join(t.TempDir(), "build-session.jsonl")}
	if err := os.MkdirAll(job.JobsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := job.Save(j); err != nil {
		t.Fatal(err)
	}
	r := &runner{job: j}

	s.freshCampaignSession(r, j.Name)

	r.mu.Lock()
	got, pin := r.job.ReviewBrief, r.job.SessionPath
	r.mu.Unlock()
	if got != brief {
		t.Fatalf("operator brief replaced: %q", got)
	}
	if pin != "" {
		t.Fatalf("build pin not cleared: %q", pin)
	}
}
