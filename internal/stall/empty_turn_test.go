package stall

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// EmptyTurn must NOT fire on a transcript that is growing (the agent is
// streaming text, just not calling tools yet).
func TestEmptyTurnIgnoresGrowingTranscript(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(p, time.Hour) // CI idle irrelevant here
	// Append prose growth, then poll so the detector sees the new bytes.
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("assistant prose streaming...\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	d.Poll()

	// Now backdate the clock: the transcript IS bigger than the baseline, so
	// growth inside the window must suppress the stall even though the
	// mtime-driven quiet timer says "2 hours".
	d.lastGrowth = time.Now().Add(-2 * time.Hour)

	stalled, _ := d.EmptyTurn(EmptyTurnWindow{Idle: time.Minute, MinGrowth: 1})
	if stalled {
		t.Fatal("empty-turn fired on a growing transcript")
	}
}

// EmptyTurn MUST fire when the transcript is frozen AND no tool call has been
// seen for the window — the lowpower-stats failure mode.
func TestEmptyTurnFiresOnSilentFrozenTranscript(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(p, time.Hour)
	d.lastGrowth = time.Now().Add(-2 * time.Hour) // frozen well past the window
	d.Poll()                                      // re-stat; size unchanged

	stalled, quiet := d.EmptyTurn(EmptyTurnWindow{Idle: time.Minute, MinGrowth: 1})
	if !stalled {
		t.Fatal("empty-turn did not fire on a silent frozen transcript")
	}
	if quiet < time.Minute {
		t.Fatalf("quietFor = %s, want >= the idle window", quiet)
	}
}

// A tool call inside the window re-arms the detector: the agent is working,
// even if the bytes are momentarily quiet.
func TestEmptyTurnRearmsOnToolCall(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(p, time.Hour)

	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"assistant","content":[{"type":"tool_use","name":"bash"}]}` + "\n")
	_ = f.Close()
	d.Poll() // reads the tool_use slice
	if d.toolCalls == 0 {
		t.Fatal("Poll did not count the tool_use marker")
	}

	d.lastGrowth = time.Now().Add(-2 * time.Hour)
	stalled, _ := d.EmptyTurn(EmptyTurnWindow{Idle: time.Minute, MinGrowth: 1 << 30})
	if stalled {
		t.Fatal("empty-turn fired despite a tool call in the window")
	}
}
