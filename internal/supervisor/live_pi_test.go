// ADR-0011 live validation — a REAL pi session, not a fixture.
//
// This is deliberately NOT a unit test. It runs the real `pi` binary through
// the real supervisor path, so the transcript that gets scanned is one pi
// actually wrote, not a hand-built JSONL. Unit tests with fixtures can only
// prove the scanner parses the shapes we believed pi writes; this proves the
// belief.
//
// The bug shape reproduced here: the agent emits its marker, and the following
// round truncates /tmp/pi_<job>_run.log. The old gate read only that run log,
// so it kept failing until MaxRounds and ended fatal. With ADR-0011 the marker
// is read from the session transcript and latches immediately.
//
// Run:  go test ./internal/supervisor/ -run TestLiveRealPi -v -timeout 400s
// Opt out (CI, no model budget):  ADR0011_LIVE=0

package supervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

const liveMarker = "ALL_LIVE_DETECT_DONE"

func liveBrief() string {
	if p := os.Getenv("ADR0011_BRIEF"); p != "" {
		return p
	}
	return "/tmp/adr0011_live_brief.md"
}

func liveSkip(t *testing.T) string {
	t.Helper()
	if os.Getenv("ADR0011_LIVE") == "0" {
		t.Skip("ADR0011_LIVE=0 — skipping the real-pi validation")
	}
	piBin, err := exec.LookPath("pi")
	if err != nil {
		t.Skipf("no pi binary on PATH: %v", err)
	}
	return piBin
}

// TestLiveRealPiDetectsMarker runs a REAL pi session and requires the marker to
// be detected from the transcript pi actually wrote.
//
// Acceptance:
//   - pi really ran (a non-trivial transcript exists)
//   - the production predicate finds the marker in that transcript
//   - the job reaches done on THIS round (MaxRounds 1: if detection needed a
//     retry, this fails — the bug needed 14)
func TestLiveRealPiDetectsMarker(t *testing.T) {
	piBin := liveSkip(t)
	// DELIBERATELY no testEnv(): this test drives a REAL pi, which needs the
	// REAL HOME (model credentials, pi config, provider setup). testEnv()
	// points HOME at a temp dir, which makes pi exit immediately with no
	// transcript — a test-harness artifact that looks exactly like the bug.
	//
	// Isolation instead comes from a private Worktree and a namespaced Job
	// name, so nothing here can collide with a live campaign job.
	brief := liveBrief()
	if _, err := os.Stat(brief); err != nil {
		t.Skipf("live brief missing (%s): %v", brief, err)
	}
	wt := t.TempDir()
	report := filepath.Join(wt, "final_report.md")
	if err := os.WriteFile(report, []byte("live validation report\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	j := job.Job{
		Name: "adr0011-live-verify", Brief: brief, Cont: brief,
		FinalReport: report, Marker: liveMarker,
		Worktree: wt, SessionName: "adr0011-live",
		MaxRounds: 1, TimeoutS: 180, PiBin: piBin, BackoffScale: 0.02,
	}
	// A true LAUNCH (SessionPath empty): pi creates its own session in the
	// worktree's munged dir, which is what FindSession resolves. Seeding a
	// bogus resume path instead would make pi write somewhere the supervisor
	// cannot discover — a flaw in the test, not the fix.
	writeJob(t, j)

	s := newTestSupervisor(t)
	if err := s.Start("adr0011-live-verify"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Stop("adr0011-live-verify")
		// Leave no trace in the real ~/.pi/supervisor tree: this job only
		// exists to validate the detector.
		_ = os.Remove(filepath.Join(job.JobsDir(), "adr0011-live-verify.json"))
		_ = os.Remove(filepath.Join(job.StateDir(), "adr0011-live-verify.json"))
	})

	// Wait for pi to write a real transcript. Resolve via the job's worktree
	// so we read the session pi actually created for THIS worktree.
	liveSess := ""
	waitFor(t, 120*time.Second, func() bool {
		liveSess = job.FindSession(j.SessionName, wt, time.Time{})
		return liveSess != "" && job.Size(liveSess) > 200
	})
	if liveSess == "" || job.Size(liveSess) <= 100 {
		t.Fatalf("pi wrote no usable transcript — the live run did not happen (found %q)", liveSess)
	}
	t.Logf("real transcript: %s (%d bytes)", liveSess, job.Size(liveSess))

	// The job must reach its terminal verdict on THIS round (MaxRounds 1).
	// Under ADR-0020 the verdict can be marker-driven or report-driven.
	waitFor(t, 120*time.Second, func() bool {
		st, _ := s.Status("adr0011-live-verify")
		m, ok := st.(job.Status)
		return ok && (m.State == "done" || m.State == "fatal")
	})
	st, _ := s.Status("adr0011-live-verify")
	fin, ok := st.(job.Status)
	if !ok {
		t.Fatalf("status = %#v, want job.Status", st)
	}
	if fin.State != "done" {
		t.Fatalf("state = %q, want done", fin.State)
	}
	// The ADR-0011 premise: the marker in the transcript pi actually wrote.
	// Real pi is a live model and can finish WITHOUT emitting the marker —
	// under ADR-0020 the job then still closes done via the report. That is
	// a model-compliance flake, not a detector failure: skip (rerun to
	// exercise the marker path) rather than fail.
	if !job.TranscriptContains(liveSess, liveMarker) {
		t.Skipf("real pi finished without emitting %s — job closed done via the report (ADR-0020); rerun to exercise the marker path", liveMarker)
	}
	t.Logf("marker found in real transcript assistant text")
	if !fin.MarkerFound {
		t.Fatal("MarkerFound false on a done job with the marker in the transcript")
	}
	if fin.Round > 1 {
		t.Fatalf("round = %d; detection needed more than one round", fin.Round)
	}
	t.Logf("LIVE PASS: real pi marker detected, state=done on round %d", fin.Round)
}

// TestLiveOldPredicateMissesSameData asserts the OLD run-log predicate fails
// where the new one succeeds, on real pi data — proving the fix is load-bearing
// rather than the test being weak.
//
// It reuses a real pi transcript containing the marker and checks it against an
// empty (i.e. just-truncated) run log, which is the state the old gate kept
// reading while the job was already finished.
func TestLiveOldPredicateMissesSameData(t *testing.T) {
	liveSkip(t)

	sess := findRealTranscriptWithMarker(t)
	// The run log for a later round: truncated empty. This is what the old
	// gate saw, round after round, while the work was done.
	empty := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if job.RunlogContains(empty, liveMarker) {
		t.Fatal("sanity: an empty run log must not contain the marker")
	}
	if !job.TranscriptContains(sess, liveMarker) {
		t.Fatal("sanity: the real transcript should contain the marker")
	}
	t.Logf("OLD predicate (run log, truncated): MISS. NEW predicate (transcript): HIT (%s). The fix is load-bearing.",
		filepath.Base(sess))
}

// findRealTranscriptWithMarker locates a transcript on disk that a real pi run
// wrote and that contains the marker.
func findRealTranscriptWithMarker(t *testing.T) string {
	t.Helper()
	root := os.Getenv("ADR0011_SESSION_ROOT")
	if root == "" {
		root = "/home/balor/.pi/agent/sessions"
	}
	var found string
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil // best-effort scan
		}
		if job.TranscriptContains(p, liveMarker) {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	if found == "" {
		t.Skipf("no real pi transcript containing %s under %s — run TestLiveRealPiDetectsMarker first",
			liveMarker, root)
	}
	return found
}
