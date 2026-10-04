package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// TestCapDiagnosticNamesMissingReportNotMarker is the regression test for the
// misleading diagnostic found live on mealime-extracats3.
//
// `done` requires BOTH the marker AND the final report (ADR-0011). The job sat
// at fatal/14 with marker_seen=true and last_diag "round cap reached without
// marker" — because the brief told pi to write
// /tmp/mealime_extracats_final_report.md while the job's final_report was
// /tmp/mealime_extracats3_final_report.md. pi did as instructed; the gate
// checked the other path.
//
// The damage was the diagnosis: an operator reading "without marker" goes
// looking for a marker that was found 13 rounds earlier, while the entire
// remedy is one `cp`. So the diagnostic must name the artifact that is
// actually missing, and point at the file if it can find it.
func TestCapDiagnosticNamesMissingReportNotMarker(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The configured report path — deliberately never created.
	report := filepath.Join(dir, "task3_final_report.md")

	j := job.Job{
		Name: "capdiag", Brief: brief, Worktree: dir, SessionName: "capdiag",
		Marker: "ALL_CAPDIAG_DONE", FinalReport: report,
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	ch, cancel, _ := s.Watch("capdiag")
	defer cancel()
	if err := s.Start("capdiag"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, func() bool { return s.RunningCount() == 1 })

	// Write the marker into the live transcript so MarkerSeen latches, but do
	// NOT create the report. This is the exact live state.
	// Let the marker watcher snap its read offset at the pre-marker file end
	// (same race the ci_stall tests document): appending earlier makes the
	// marker "historical" so the streaming latch never arms.
	waitFor(t, 30*time.Second, func() bool { return s.RunningCount() == 1 })
	time.Sleep(500 * time.Millisecond)

	waitFor(t, 30*time.Second, func() bool {
		r := s.jobs["capdiag"]
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
		_, _ = f.WriteString(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"ALL_CAPDIAG_DONE"}]}}` + "\n")
		return true
	})

	var info string
	deadline := time.After(90 * time.Second)
	for info == "" {
		select {
		case e := <-ch:
			if e.Event == "fatal" {
				info = e.Info
			}
		case <-deadline:
			t.Fatal("no fatal within 90s")
		}
	}

	// The report path must be named: that is the whole remedy.
	if !strings.Contains(info, report) {
		t.Fatalf("fatal info does not name the missing report path %q:\n%s", report, info)
	}
	// And it must NOT blame the marker, which was seen.
	if strings.Contains(info, "without marker") {
		t.Fatalf("fatal info blames the marker although MarkerSeen was true:\n%s", info)
	}
	// And the state file must agree.
	st, err := job.LoadState("capdiag")
	if err != nil {
		t.Fatal(err)
	}
	if !st.MarkerSeen {
		t.Fatal("precondition: the marker should have been seen")
	}
	if !strings.Contains(st.LastDiag, report) {
		t.Fatalf("last_diag does not name the missing report: %q", st.LastDiag)
	}
}

// TestCapDiagnosticFindsMisnamedReport covers the second half of the fix: when
// the report exists but under a different version digit (the live
// mealime-extracats3 case), the diagnostic must point at the file it found, so
// the remedy is a copy-paste rather than an `ls` hunt through /tmp.
func TestCapDiagnosticFindsMisnamedReport(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Configured path has a "3"; the brief's path (what pi actually writes)
	// does not.
	report := filepath.Join(dir, "mealime_extracats3_final_report.md")
	actual := filepath.Join(dir, "mealime_extracats_final_report.md")

	j := job.Job{
		Name: "capfind", Brief: brief, Worktree: dir, SessionName: "capfind",
		Marker: "ALL_CAPFIND_DONE", FinalReport: report,
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	ch, cancel, _ := s.Watch("capfind")
	defer cancel()
	if err := s.Start("capfind"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, func() bool { return s.RunningCount() == 1 })

	waitFor(t, 30*time.Second, func() bool { return s.RunningCount() == 1 })
	time.Sleep(500 * time.Millisecond)

	// Written AFTER launch: the lookup is scoped to files newer than the job
	// started, so a pre-existing file is correctly rejected as stale.
	if err := os.WriteFile(actual, []byte("the report\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 30*time.Second, func() bool {
		r := s.jobs["capfind"]
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
		_, _ = f.WriteString(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"ALL_CAPFIND_DONE"}]}}` + "\n")
		return true
	})

	var info string
	deadline := time.After(90 * time.Second)
	for info == "" {
		select {
		case e := <-ch:
			if e.Event == "fatal" {
				info = e.Info
			}
		case <-deadline:
			t.Fatal("no fatal within 90s")
		}
	}

	if !strings.Contains(info, actual) {
		t.Fatalf("fatal info does not point at the report it found (%q):\n%s", actual, info)
	}
	if !strings.Contains(info, "FOUND at") {
		t.Fatalf("fatal info should say it FOUND an alternative:\n%s", info)
	}
}
