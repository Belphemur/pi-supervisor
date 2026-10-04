package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// TestCapDiagnosticAcceptsRunlogMarker covers the second review finding on the
// cap diagnostic: it read only r.state.MarkerSeen, which is written solely by
// the STREAMING transcript watcher. The completion gate also accepts a marker
// found by a cumulative transcript scan or by the round's run log, and neither
// writes that result back to the latch.
//
// So for a legitimate case — pi announces completion on stdout with no
// assistant record — the gate latches completion while the diagnostic reported
// "marker NOT seen AND final report missing". That is precisely the
// wrong-cause diagnosis the switch was added to eliminate, reintroduced through
// a different surface.
//
// The assertion: the marker exists ONLY in the run log, never in the
// transcript, and the diagnostic must still say the marker WAS seen.
func TestCapDiagnosticAcceptsRunlogMarker(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Configured report path, never created: so the report IS the blocker and
	// the diagnostic must name it while crediting the marker.
	report := filepath.Join(dir, "task3_final_report.md")
	marker := "ALL_RUNLOGMARK_DONE"

	j := job.Job{
		Name: "runlogmark", Brief: brief, Worktree: dir, SessionName: "runlogmark",
		Marker: marker, FinalReport: report,
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	ch, cancel, _ := s.Watch("runlogmark")
	defer cancel()
	if err := s.Start("runlogmark"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, func() bool { return s.RunningCount() == 1 })

	// Put the marker in the RUN LOG only — never in the transcript. This is
	// the surface the latch does not observe.
	//
	// Written in a goroutine AFTER the round is live: round() truncates the
	// run log at the start of every round (ADR-0011 invariant 5), so a write
	// made before the spawn is discarded and the marker never exists for the
	// gate to find.
	runlog := job.Runlog("runlogmark")
	go func() {
		time.Sleep(2 * time.Second)
		f, err := os.OpenFile(runlog, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()
		_, _ = f.WriteString("work finished\n" + marker + "\n")
	}()

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

	// The transcript must genuinely lack the marker, or the test proves nothing.
	if job.TranscriptContains(j.SessionPath, marker) {
		t.Skip("transcript picked up the marker; this case needs a runlog-only marker")
	}

	if strings.Contains(info, "marker NOT seen") {
		t.Fatalf("diagnostic claims the marker was not seen, but the gate accepts a runlog marker:\n%s", info)
	}
	if !strings.Contains(info, "marker WAS seen") {
		t.Fatalf("diagnostic should credit the marker as seen:\n%s", info)
	}
	if !strings.Contains(info, report) {
		t.Fatalf("diagnostic must name the missing report %q:\n%s", report, info)
	}
}
