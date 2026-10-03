package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-supervisor/internal/job"
)

// The mealime-roomux bug (ADR-0011), reproduced at the supervisor level.
//
// round() truncates /tmp/pi_<job>_run.log at the start of every round, while
// the marker gate asked that per-round artifact a cumulative question. So a
// marker emitted in an early round was structurally invisible, every later
// round re-ran the full prompt, and the job ended fatal with
// "round cap reached without marker" — with the work finished and the PR open.
//
// These tests drive the GATE directly: the gate is the thing that was wrong.

// gateMarkerSeen reports what the marker gate computes for a runner, using the
// same inputs the loop uses: the sticky MarkerSeen latch, a cumulative
// transcript scan, and the final_report existence check.
func gateMarkerSeen(markerSeen bool, sess, marker, finalReport, runlog string) bool {
	if marker == "" {
		return false // an empty marker must never reach "contains"
	}
	seen := markerSeen
	if !seen && sess != "" {
		seen = job.TranscriptContains(sess, marker)
	}
	if !seen {
		seen = job.RunlogContains(runlog, marker)
	}
	return seen && job.Exists(finalReport)
}

// assistantRecord builds one real persisted-session record: an assistant
// message whose content is a block array (the shape pi actually writes).
func assistantRecord(text string) string {
	b, err := json.Marshal(text)
	if err != nil {
		panic(err)
	}
	return `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":` +
		string(b) + `}]}}`
}

// appendFile appends to a file (transcripts grow; they are not rewritten).
func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// THE REGRESSION: the marker is in an assistant message in the transcript, the
// run log has been truncated to nothing (as round() does every round), and the
// final report exists. The gate must say YES.
func TestGateFindsMarkerInTranscriptAfterRunlogTruncated(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	report := filepath.Join(dir, "final.md")

	// The marker round: pi finished and said so.
	if err := os.WriteFile(sess, []byte(assistantRecord("done — ALL_MEALIME_ROOMUX_DONE")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("report"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The run log for the CURRENT round: truncated empty, no marker. This is
	// what the old gate read, which is why it kept failing.
	runlog := filepath.Join(dir, "run.log")
	if err := os.WriteFile(runlog, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if job.RunlogContains(runlog, "ALL_MEALIME_ROOMUX_DONE") {
		t.Fatal("precondition: runlog must NOT contain the marker")
	}
	if !gateMarkerSeen(false, sess, "ALL_MEALIME_ROOMUX_DONE", report, runlog) {
		t.Fatal("gate missed a marker that is plainly in the transcript — the mealime-roomux bug")
	}
}

// The latch alone is enough (the streaming path): even if the transcript has
// since been rotated away, a marker already seen keeps the job completable.
func TestGateHonoursStickyMarkerSeen(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "final.md")
	if err := os.WriteFile(report, []byte("r"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !gateMarkerSeen(true, "", "MK", report, filepath.Join(dir, "empty.log")) {
		t.Fatal("a latched marker was not honored")
	}
}

// final_report is still REQUIRED: a marker alone must not finish the job, or a
// stale report check is dropped and partial runs get declared done.
func TestGateRequiresFinalReport(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(sess, []byte(assistantRecord("ALL_DONE")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if gateMarkerSeen(false, sess, "ALL_DONE", filepath.Join(dir, "missing.md"), filepath.Join(dir, "empty.log")) {
		t.Fatal("gate finished a job with no final report")
	}
}

// A DCP/user/toolCall echo of the marker is NOT completion.
func TestGateIgnoresNonAssistantMarkerEcho(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	report := filepath.Join(dir, "final.md")
	if err := os.WriteFile(report, []byte("r"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := strings.Join([]string{
		`{"type":"custom","customType":"dcp-state","data":{"summary":"emit ALL_DONE when finished"}}`,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"end with ALL_DONE"}]}}`,
		assistantRecord("still working on it"),
	}, "\n") + "\n"
	if err := os.WriteFile(sess, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if gateMarkerSeen(false, sess, "ALL_DONE", report, filepath.Join(dir, "empty.log")) {
		t.Fatal("an echo of the marker was mistaken for completion")
	}
}

// An empty marker never completes anything (strings.Contains(x,"") is true).
// The run log REMAINS a valid signal for the round that produced it: an agent
// can announce completion on stdout without persisting an assistant record
// (the fake pi in the integration tests does exactly this). Dropping it
// regressed TestMarkerGateFinishesJob and TestStartRefusesFinishedJob — this
// test exists so it cannot be dropped again silently.
func TestGateStillAcceptsRunlogMarkerWithinTheRound(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	report := filepath.Join(dir, "final.md")
	runlog := filepath.Join(dir, "run.log")
	if err := os.WriteFile(sess, []byte(`{"type":"session"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("r"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Marker on stdout this round, nothing in the transcript.
	if err := os.WriteFile(runlog, []byte("streaming text MARKER_STDOUT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !gateMarkerSeen(false, sess, "MARKER_STDOUT", report, runlog) {
		t.Fatal("a marker emitted on stdout in THIS round was rejected")
	}
}

// But the run log must never be sufficient ACROSS rounds — that is the bug.
// Truncating it (as round() does) must make the gate fall back to the
// transcript, which still holds the marker.
func TestGateRunlogTruncationFallsBackToTranscript(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	report := filepath.Join(dir, "final.md")
	runlog := filepath.Join(dir, "run.log")
	// Transcript still holds the marker from an earlier round.
	if err := os.WriteFile(sess, []byte(assistantRecord("done earlier MARKER_X")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("r"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The current round truncated it.
	if err := os.WriteFile(runlog, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !gateMarkerSeen(false, sess, "MARKER_X", report, runlog) {
		t.Fatal("transcript fallback failed after the runlog was truncated")
	}
	// And with neither surface, it must NOT complete.
	if gateMarkerSeen(false, sess, "MARKER_ABSENT", report, runlog) {
		t.Fatal("gate completed with the marker on no surface")
	}
}

func TestGateEmptyMarkerNeverCompletes(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	report := filepath.Join(dir, "final.md")
	if err := os.WriteFile(sess, []byte(assistantRecord("anything")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("r"), 0o644); err != nil {
		t.Fatal(err)
	}
	if gateMarkerSeen(false, sess, "", report, filepath.Join(dir, "empty.log")) {
		t.Fatal("an empty marker completed the job")
	}
}

// watchMarker must latch mid-round: the marker arrives while pi is still
// running, and the runner's MarkerSeen must flip without waiting for the round
// to end. This is the "stream and keep parsing as we stream" requirement.
func TestWatchMarkerLatchesMidRound(t *testing.T) {
	testEnv(t)
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	report := filepath.Join(dir, "final.md")
	if err := os.WriteFile(sess, []byte(`{"type":"session","id":"a"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("r"), 0o644); err != nil {
		t.Fatal(err)
	}

	j := job.Job{Name: "wm", SessionPath: sess, SessionName: "wm", Worktree: dir,
		Marker: "ALL_DONE", FinalReport: report, MaxRounds: 1, TimeoutS: 20}
	writeJob(t, j)
	s := newTestSupervisor(t)
	r := &runner{job: j, state: job.State{Round: 1, State: "running"}, stopCh: make(chan struct{})}
	s.jobs["wm"] = r

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); s.watchMarker(r, sess, "ALL_DONE", 1, stop, r.stopCh) }()

	// pi is mid-turn: no marker yet.
	time.Sleep(100 * time.Millisecond)
	if r.stateSnapshot().MarkerSeen {
		t.Fatal("latched with no marker written")
	}

	// pi finishes and emits the marker.
	appendFile(t, sess, assistantRecord("ALL_DONE")+"\n")

	// The watcher must latch on its own, without the round ending.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r.stateSnapshot().MarkerSeen {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(stop)
	<-done
	if !r.stateSnapshot().MarkerSeen {
		t.Fatal("watchMarker did not latch the marker while the round was live")
	}
	// And the gate now says done.
	if !gateMarkerSeen(r.stateSnapshot().MarkerSeen, sess, "ALL_DONE", report, filepath.Join(dir, "empty.log")) {
		t.Fatal("gate refused a latched marker with a final report present")
	}
}

// watchMarker must do nothing when no marker is configured.
func TestWatchMarkerNoMarkerConfigured(t *testing.T) {
	testEnv(t)
	dir := t.TempDir()
	sess := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(sess, []byte(assistantRecord("ALL_DONE")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{Name: "nm", SessionPath: sess, SessionName: "nm", Worktree: dir, MaxRounds: 1}
	writeJob(t, j)
	s := newTestSupervisor(t)
	r := &runner{job: j, state: job.State{Round: 1}, stopCh: make(chan struct{})}
	s.jobs["nm"] = r

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); s.watchMarker(r, sess, "", 1, stop, r.stopCh) }()
	time.Sleep(200 * time.Millisecond)
	close(stop)
	<-done
	if r.stateSnapshot().MarkerSeen {
		t.Fatal("latched with no marker configured")
	}
}
