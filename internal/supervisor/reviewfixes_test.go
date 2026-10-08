package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// TestUserQuoteMarkerDoesNotArmReportRequests (qodo PR#7 finding 3): the brief
// tells the agent which marker to emit and pi records the brief as a USER
// message. A marker that appears ONLY in user-message quotes is not the agent
// signaling completion — it must not arm report requests on a healthy
// multi-round job, and the run must reach its cap without any ask.
func TestUserQuoteMarkerDoesNotArmReportRequests(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "final_report.md") // never created

	j := job.Job{
		Name: "userquote", Brief: brief, Worktree: dir, SessionName: "userquote",
		Marker: "ALL_USERQUOTE_DONE", FinalReport: report,
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	// The marker ONLY as a user-message quote (the brief echoing): the
	// agent-authored surfaces stay empty for the whole run.
	sess := j.SessionPath
	userRec := `{"type":"message","message":{"role":"user","content":[{"type":"text","text":"end your last message with ALL_USERQUOTE_DONE"}]}}` + "\n"
	if err := os.WriteFile(sess, []byte("{}\n"+userRec), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	if err := s.Start("userquote"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 90*time.Second, func() bool {
		st, _ := job.LoadState("userquote")
		return st.State == "fatal"
	})

	st, err := job.LoadState("userquote")
	if err != nil {
		t.Fatal(err)
	}
	if st.ReportSteers != 0 {
		t.Errorf("ReportSteers = %d, want 0 — a user-message quote is not agent intent", st.ReportSteers)
	}
	if st.MarkerSeen {
		t.Error("MarkerSeen latched on a user-message quote — only assistant text counts")
	}
	if !strings.Contains(st.LastDiag, report) {
		t.Errorf("last_diag does not name the missing report: %q", st.LastDiag)
	}
}

// TestRestartFreshQuarantinesStaleReport (qodo PR#7 finding 4 / kody): a
// report left by the previous run must not satisfy the fresh run's mtime
// floor — restart --fresh moves it to _archived-stale next to the old file,
// the same treatment the session transcript gets (ADR-0010).
func TestRestartFreshQuarantinesStaleReport(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "final_report.md")
	if err := os.WriteFile(report, []byte("previous run's report\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	j := job.Job{
		Name: "restartrep", Brief: brief, Worktree: dir, SessionName: "restartrep",
		Marker: "ALL_RESTARTREP_DONE", FinalReport: report,
		MaxRounds: 3, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	if err := s.Restart("restartrep"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop("restartrep") })

	if _, err := os.Stat(report); !os.IsNotExist(err) {
		t.Fatal("the stale report was not quarantined out of the live path")
	}
	matches, err := filepath.Glob(filepath.Join(dir, "_archived-stale", "final_report.md_*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("quarantined report not found exactly once in _archived-stale: %v (%v)", matches, err)
	}
}
