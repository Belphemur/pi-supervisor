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
	for range 4 {
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

// Real transcript shapes (from the live enum session): one tool_use block per
// assistant line, one toolResult line per returned call — and that result
// line carries BOTH "role":"toolResult" and "toolCallId". The old raw
// substring count matched the same line twice and drained toolsInFlight at
// double rate, silently weakening the never-abort-a-running-tool guarantee
// (commit 18d48d4). One result line must decrement exactly once.
func TestToolResultCountsOncePerLine(t *testing.T) {
	use := `{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"bash"}]}`
	result := `{"role":"toolResult","toolCallId":"call_1","toolName":"bash"}`

	// Directly: one result line is ONE result, however many of its fields
	// mention it.
	if got := countResultLines([]byte(result + "\n")); got != 1 {
		t.Fatalf("countResultLines(real toolResult line) = %d, want 1", got)
	}
	// The Anthropic-shaped equivalent counts once too.
	if got := countResultLines([]byte(`{"type":"tool_result","tool_use_id":"call_1"}` + "\n")); got != 1 {
		t.Fatalf("countResultLines(anthropic result line) = %d, want 1", got)
	}
	// Prose merely MENTIONING a result is not one.
	if got := countResultLines([]byte(`{"content":"the toolResult arrived"}` + "\n")); got != 0 {
		t.Fatalf("countResultLines(prose mention) = %d, want 0", got)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(path, time.Second)
	app := func(line string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(line + "\n")
		_ = f.Close()
	}

	// Three concurrent calls go out (three lines; parallel calls in one
	// content array would count per occurrence, but one-per-line is the
	// common shape).
	for range 3 {
		app(use)
	}
	if _, _ = d.Poll(); d.toolsInFlight != 3 {
		t.Fatalf("toolsInFlight after 3 issued calls = %d, want 3", d.toolsInFlight)
	}

	// ONE call returns. The counter must drop by exactly one — with the
	// double-count it dropped by two and a still-running pair looked idle.
	app(result)
	if _, _ = d.Poll(); d.toolsInFlight != 2 {
		t.Fatalf("toolsInFlight after 1 of 3 returned = %d, want 2 (double-count drains too fast)", d.toolsInFlight)
	}
}

// Two tool_use blocks in ONE assistant line (parallel tool calls) are two
// calls, counted per occurrence; the result side stays per-line.
func TestParallelToolUsesCountPerOccurrence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(path, time.Second)
	app := func(line string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(line + "\n")
		_ = f.Close()
	}
	app(`{"role":"assistant","content":[{"type":"tool_use","id":"a"},{"type":"tool_use","id":"b"}]}`)
	if _, _ = d.Poll(); d.toolsInFlight != 2 {
		t.Fatalf("toolsInFlight after one line with 2 parallel calls = %d, want 2", d.toolsInFlight)
	}
}

// A rotation must reset the tool counters with everything else: counts that
// describe the OLD file must not mute EmptyTurn against the new one. Before
// the fix a call seen in flight just before the rotation pinned
// toolsInFlight above zero for the rest of the round.
func TestRotationResetsToolCounters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(path, time.Second)
	app := func(line string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(line + "\n")
		_ = f.Close()
	}

	app(`{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"bash"}]}`)
	if _, _ = d.Poll(); d.toolsInFlight != 1 {
		t.Fatalf("toolsInFlight before rotation = %d, want 1", d.toolsInFlight)
	}

	// Rotate: the file shrinks (pi restarts it), then stays quiet past the
	// empty-turn window.
	if err := os.WriteFile(path, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _ = d.Poll(); d.toolsInFlight != 0 || d.toolCalls != 0 {
		t.Fatalf("after rotation toolsInFlight=%d toolCalls=%d, want 0/0 — stale counts mute EmptyTurn", d.toolsInFlight, d.toolCalls)
	}

	// The detector must actually be able to fire again on the new file.
	time.Sleep(30 * time.Millisecond)
	stalled, _ := d.EmptyTurn(EmptyTurnWindow{Idle: 20 * time.Millisecond, MinGrowth: 1})
	if !stalled {
		t.Fatal("EmptyTurn stayed muted after a rotation reset the counters")
	}
}
