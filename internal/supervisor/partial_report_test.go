package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// TestPartialReportNeverMarksDone is the end-to-end regression for the real
// false-positive completion on mealime-userrecipes (2026-10-05).
//
// The job reported `state: done`, `marker_found: true`, `final_report_exists:
// true` while T6-T9 were unimplemented, because the agent wrote
// "ALL_MEALIME_USERRECIPES_DONE deliberately NOT emitted" in an assistant
// message and TranscriptContains was a bare substring match. Four rounds of real
// work were discarded because a done job stops looping.
//
// Two independent signals now have to agree that work is finished, and a report
// that declares itself incomplete actively BLOCKS completion.
func TestPartialReportNeverMarksDone(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The real report, verbatim in substance.
	report := filepath.Join(dir, "final_report.md")
	body := "# Report\n\n**Status: PARTIAL.** Tasks T1-T5 are green. T6-T9 are\nNOT done.\n\n" +
		"## Repo facts a follow-up run needs\nBranch is behind origin/main.\n\n" +
		"ALL_PARTIAL_DONE deliberately NOT emitted: T6-T9 are incomplete.\n"
	if err := os.WriteFile(report, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	j := job.Job{
		Name: "partial", Brief: brief, Worktree: dir, SessionName: "partial",
		Marker: "ALL_PARTIAL_DONE", FinalReport: report,
		MaxRounds: 2, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	ch, cancel, _ := s.Watch("partial")
	defer cancel()
	if err := s.Start("partial"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, func() bool { return s.RunningCount() == 1 })

	// Put the quoted marker in an assistant message — the exact false positive.
	waitFor(t, 30*time.Second, func() bool {
		r := s.jobs["partial"]
		r.mu.Lock()
		sess := r.job.SessionPath
		r.mu.Unlock()
		if sess == "" {
			return false
		}
		f, err := os.OpenFile(sess, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return false
		}
		defer func() { _ = f.Close() }()
		_, _ = f.WriteString(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"ALL_PARTIAL_DONE deliberately NOT emitted: T6-T9 are incomplete."}]}}` + "\n")
		return true
	})

	// The job must NOT report done.
	deadline := time.After(60 * time.Second)
	for {
		st, err := job.LoadState("partial")
		if err == nil && st.State == "done" {
			t.Fatalf("FALSE POSITIVE REACHED done: the report declares PARTIAL and the marker was only quoted")
		}
		select {
		case e := <-ch:
			if e.Event == "done" {
				t.Fatalf("emitted done for a partial report: %s", e.Info)
			}
			if e.Event == "report_incomplete" {
				if !strings.Contains(e.Info, "PARTIAL") && !strings.Contains(e.Info, "INCOMPLETE") {
					t.Errorf("report_incomplete should quote the declaration: %q", e.Info)
				}
				// The decisive assertion: state must not be done.
				if st, _ := job.LoadState("partial"); st.State == "done" {
					t.Fatal("state is done after report_incomplete")
				}
				return
			}
		case <-deadline:
			t.Fatal("neither done nor report_incomplete within 60s")
		}
	}
}
