package stall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A CI marker followed by transcript quiet for the idle window = stall;
// the detector then re-arms and can fire again on a later park.
func TestPollStallCycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(path, 300*time.Millisecond)

	// Historical content (before New) never arms.
	if stalled, _ := d.Poll(); stalled {
		t.Fatal("armed on pre-existing content")
	}

	app := func(line string) {
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		_, _ = f.WriteString(line + "\n")
		_ = f.Close()
	}

	app(`{"type":"message","role":"assistant","content":"10 checks running. Let me wait for CI and the re-review to land."}`)
	if stalled, _ := d.Poll(); stalled {
		t.Fatal("stall before the idle window (marker should only arm)")
	}

	time.Sleep(350 * time.Millisecond)
	stalled, marker := d.Poll()
	if !stalled {
		t.Fatal("expected a stall after the idle window")
	}
	lm := strings.ToLower(marker)
	if !strings.Contains(lm, "checks") && !strings.Contains(lm, "review") &&
		!strings.Contains(lm, "ci") && !strings.Contains(lm, "pipeline") {
		t.Fatalf("marker %q is not a CI/review phrase", marker)
	}

	// Re-armed: immediate poll is quiet, no double-fire.
	if stalled, _ := d.Poll(); stalled {
		t.Fatal("double-fired without a new marker")
	}
}

// New transcript activity resets the idle timer: a long build emitting output
// must not count as parked on CI.
func TestActivityResetsIdle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(path, 200*time.Millisecond)
	app := func(line string) {
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		_, _ = f.WriteString(line + "\n")
		_ = f.Close()
	}
	app(`{"content":"gh pr checks — waiting for CI"}`)
	_, _ = d.Poll()
	// Keep the transcript alive through the idle window.
	for i := 0; i < 4; i++ {
		time.Sleep(80 * time.Millisecond)
		app(`{"content":"tool output still streaming"}`)
		_, _ = d.Poll()
	}
	if stalled, _ := d.Poll(); stalled {
		t.Fatal("stalled despite continuous activity")
	}
}

// The answer-code-review skill path is a marker (owner's standing workflow).
func TestAnswerCodeReviewMarker(t *testing.T) {
	if !ciRe.MatchString("/answer-code-review landed comments") {
		t.Fatal("answer-code-review not detected")
	}
	if !ciRe.MatchString("Waiting for the pipeline to finish") {
		t.Fatal("pipeline wait not detected")
	}
	if ciRe.MatchString("I refactored the review module") {
		t.Fatal("false positive on ordinary prose mentioning review")
	}
}
