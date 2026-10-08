package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// ADR-0020 replay tests: the real position-restore session (2026-10-08,
// Belphemur/XPoint PR #203) is streamed back through the supervisor by a fake
// pi — no real pi, no network. The fixture
// (testdata/replay-position-restore.jsonl) carries the load-bearing records
// of the REAL problematic transcript: the marker appears in the agent's
// THINKING blocks, in a `write` toolCall's arguments, in a compaction summary
// and user-message quotes — and in ZERO assistant text blocks. Under the
// ADR-0011 conjunction that job burned 6 verification rounds and died fatal
// ("round cap 12 reached without marker") with the final report fully
// written and ending in the marker.

// replayPi builds a pi replacement that streams a fixture into a pinned
// session. The client spawns PiBin directly, so the session/fixture paths are
// injected through a tiny wrapper script that sets the replay env before
// exec'ing the shared fake-pi.py (which handles TEST_REPLAY prompts). sleepS
// keeps each replayed turn open that long, so a steer delivered into the
// round is polled off the control file before the turn ends.
func replayPi(t *testing.T, sessPath, fixture string, sleepS float64) string {
	t.Helper()
	src := fakePiPath(t)
	bin := filepath.Join(t.TempDir(), "replay-pi.sh")
	script := fmt.Sprintf("#!/bin/sh\nexport REPLAY_SESSION=%q\nexport REPLAY_FIXTURE=%q\nexport REPLAY_SLEEP=%g\nexec python3 %s \"$@\"\n",
		sessPath, fixture, sleepS, src)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func replayFixturePath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../testdata/replay-position-restore.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("replay fixture missing: %v", err)
	}
	return p
}

// TestReplayPositionRestoreReportDrivenDone replays the problematic session
// with the final report present: under ADR-0020 the fully written report
// closes the job DONE at the first round boundary — no marker in assistant
// text, no fatal, none of the 6 wasted verification rounds the real run
// burned before this fix.
func TestReplayPositionRestoreReportDrivenDone(t *testing.T) {
	testEnv(t)
	// The replay must stream into the SAME file the supervisor pins, or the
	// fixture lands in a session nobody watches.
	sess := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(sess, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pi := replayPi(t, sess, replayFixturePath(t), 0.2)

	report := filepath.Join(t.TempDir(), "final_report.md")
	brief := filepath.Join(t.TempDir(), "cont.txt")
	if err := os.WriteFile(brief, []byte("TEST_REPLAY"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "replaydone", Brief: brief, Worktree: t.TempDir(), SessionName: "replaydone",
		Marker: "TTF_POSITION_RESTORE_DONE", FinalReport: report,
		MaxRounds: 2, TimeoutS: 30, PiBin: pi, BackoffScale: 0.02,
		SessionPath: sess, Cont: brief,
	}
	writeJob(t, j)

	// The report the agent "wrote", ending in the marker exactly like the
	// real one did. Fresh mtime: reportReady's StartedAt floor must accept it.
	if err := os.WriteFile(report, []byte("# Report\n\nAll gates green.\n\nTTF_POSITION_RESTORE_DONE\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestSupervisor(t)
	ch, cancel, _ := s.Watch("replaydone")
	defer cancel()
	if err := s.Start("replaydone"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, func() bool {
		st, err := job.LoadState("replaydone")
		return err == nil && st.State == "done"
	})
	// The close is announced as an event; the events broker never closes the
	// channel on cancel, so drain non-blockingly with a small settle window.
	var doneInfo string
	deadline := time.Now().Add(5 * time.Second)
	for doneInfo == "" && time.Now().Before(deadline) {
		select {
		case ev := <-ch:
			if ev.Event == "done" {
				doneInfo = ev.Info
			}
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	st, err := job.LoadState("replaydone")
	if err != nil {
		t.Fatal(err)
	}
	if st.ReportSteers != 0 {
		t.Errorf("report_steers = %d, want 0 (report was already complete)", st.ReportSteers)
	}
	if st.MarkerSeen {
		t.Errorf("MarkerSeen latched true, but the fixture has NO assistant text marker — the done must be report-driven")
	}
	if !strings.Contains(doneInfo, "final report") {
		t.Errorf("done event %q must name the report-driven close (ADR-0020)", doneInfo)
	}
}

// TestReplayPositionRestoreMarkerWithoutReportAsksTwice replays the same
// session WITHOUT the report: the marker-on-any-surface signal must make the
// daemon ask for the report twice (report_requested events, report_steers=2
// in persisted state) and then close fatal naming the missing artifact —
// instead of the old behavior of silently looping verification rounds to the
// cap.
func TestReplayPositionRestoreMarkerWithoutReportAsksTwice(t *testing.T) {
	testEnv(t)
	// Delivery must land inside a round: shrink the client's ctrl-file poll
	// to milliseconds like the steer tests do.
	oldPoll := controlPollInterval
	controlPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { controlPollInterval = oldPoll })
	// Same pinned-session discipline as the done test.
	sess := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(sess, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pi := replayPi(t, sess, replayFixturePath(t), 3) // long turns: the steer must be polled mid-round

	report := filepath.Join(t.TempDir(), "final_report.md") // NEVER created
	brief := filepath.Join(t.TempDir(), "cont.txt")
	if err := os.WriteFile(brief, []byte("TEST_REPLAY"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "replayask", Brief: brief, Worktree: t.TempDir(), SessionName: "replayask",
		Marker: "TTF_POSITION_RESTORE_DONE", FinalReport: report,
		// ask #1 armed at round-1 boundary, delivered in round 2; ask #2
		// armed at round-2 boundary, delivered in round 3; fatal at the
		// round-3 boundary. 5 rounds of headroom.
		MaxRounds: 5, TimeoutS: 30, PiBin: pi, BackoffScale: 0.02,
		SessionPath: sess, Cont: brief,
	}
	writeJob(t, j)

	s := newTestSupervisor(t)
	ch, cancel, _ := s.Watch("replayask")
	defer cancel()

	if err := s.Start("replayask"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 120*time.Second, func() bool {
		st, err := job.LoadState("replayask")
		return err == nil && st.State == "fatal"
	})
	cancel()
	// Drain non-blockingly: the fatal event lands around persistState, so
	// give the buffered events a brief settle window. The channel is never
	// closed by cancel (the broker only deletes the subscription).
	var asks int
	deadline := time.Now().Add(5 * time.Second)
	for asks < reportSteerMax && time.Now().Before(deadline) {
		select {
		case ev := <-ch:
			if ev.Event == "report_requested" {
				asks++
			}
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	st, err := job.LoadState("replayask")
	if err != nil {
		t.Fatal(err)
	}
	if st.ReportSteers != reportSteerMax {
		t.Errorf("report_steers = %d, want %d — the daemon must have actually delivered %d report requests",
			st.ReportSteers, reportSteerMax, reportSteerMax)
	}
	if !strings.Contains(st.LastDiag, "still missing after 2 report requests") {
		t.Errorf("fatal diag %q must name the missing artifact and the ask count", st.LastDiag)
	}
	if asks != reportSteerMax {
		t.Errorf("report_requested events = %d, want %d", asks, reportSteerMax)
	}
}

// TestReplayPositionRestoreOldLogicWouldBurnRounds is the control: with the
// marker present only in thinking blocks and the report MISSING, the OLD
// gate (marker AND report, no ask) could only loop to the cap. The new ask
// path must fire BEFORE the cap — asserted implicitly by the fatal arriving
// with MaxRounds=5 while the old path needed the full cap. Here we assert
// the fatal is the report-ask fatal, NOT the cap fatal.
func TestReplayPositionRestoreFatalIsAskFatalNotCapFatal(t *testing.T) {
	testEnv(t)
	oldPoll := controlPollInterval
	controlPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { controlPollInterval = oldPoll })
	sess := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(sess, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pi := replayPi(t, sess, replayFixturePath(t), 3)

	report := filepath.Join(t.TempDir(), "final_report.md")
	brief := filepath.Join(t.TempDir(), "cont.txt")
	if err := os.WriteFile(brief, []byte("TEST_REPLAY"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "replaynotcap", Brief: brief, Worktree: t.TempDir(), SessionName: "replaynotcap",
		Marker: "TTF_POSITION_RESTORE_DONE", FinalReport: report,
		MaxRounds: 5, TimeoutS: 30, PiBin: pi, BackoffScale: 0.02,
		SessionPath: sess, Cont: brief,
	}
	writeJob(t, j)

	s := newTestSupervisor(t)
	if err := s.Start("replaynotcap"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 120*time.Second, func() bool {
		st, err := job.LoadState("replaynotcap")
		return err == nil && st.State == "fatal"
	})
	st, _ := job.LoadState("replaynotcap")
	if st.Round > reportSteerMax+1 {
		t.Errorf("job ran %d rounds before fatal — the ask path must close it at ask %d + 1, not loop to the cap",
			st.Round, reportSteerMax)
	}
	if strings.Contains(st.LastDiag, "round cap") {
		t.Errorf("fatal diag %q is the OLD cap fatal — the report-ask path should have fired first", st.LastDiag)
	}
}
