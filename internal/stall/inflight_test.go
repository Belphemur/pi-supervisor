package stall

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// toolUseLine is an assistant message requesting a tool.
const toolUseLine = `{"type":"message","message":{"role":"assistant","content":[{"type":"tool_use","name":"bash","id":"toolu_01"}]}}`

// toolResultLine is the matching return.
const toolResultLine = `{"type":"message","message":{"role":"toolResult","toolCallId":"call_01","toolName":"bash","content":[]}}`

// TestEmptyTurnDoesNotFireWhileToolInFlight is the regression test for the
// defect found live on the enum job: the empty-turn detector aborted a turn
// that was actively running a long tool, surfacing as exit 130 (SIGINT) with a
// perfectly healthy transcript.
//
// A long `go test -race` freezes the transcript for its whole duration, which
// is indistinguishable from a hang by growth alone. The detector used to treat
// "a tool call was seen" as a ONE-TIME reprieve and then zeroed the counter,
// so the next tick after the window elapsed aborted the still-running tool.
//
// This test fails against that behavior: the tool is issued, never returns,
// the window elapses, and the detector must keep reporting "not stalled" on
// EVERY evaluation, not just the first.
func TestEmptyTurnDoesNotFireWhileToolInFlight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(path, time.Minute)

	// The agent issues a long-running tool.
	writeLines(t, path, toolUseLine)
	if _, _ = d.Poll(); false {
		t.Fatal("unreachable")
	}

	w := EmptyTurnWindow{Idle: 10 * time.Millisecond, MinGrowth: 1}

	// Age the detector past the idle window.
	d.lastGrowth = time.Now().Add(-time.Minute)

	// Repeatedly: a tool is outstanding, so this must NEVER be a stall. The
	// old code passed the first check and failed the second — that is the bug.
	for i := range 5 {
		stalled, _ := d.EmptyTurn(w)
		if stalled {
			t.Fatalf("evaluation %d: detector aborted a turn with a tool still in flight", i)
		}
		time.Sleep(15 * time.Millisecond)
		d.lastGrowth = time.Now().Add(-time.Minute) // stay past the window
	}
}

// TestEmptyTurnFiresAfterToolReturns is the other half: the in-flight
// suppression must not become a permanent mute. Once the call comes back and
// the agent goes genuinely quiet, the stall must fire.
func TestEmptyTurnFiresAfterToolReturns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(path, time.Minute)

	writeLines(t, path, toolUseLine)
	d.Poll()
	w := EmptyTurnWindow{Idle: 10 * time.Millisecond, MinGrowth: 1 << 40} // effectively frozen

	d.lastGrowth = time.Now().Add(-time.Minute)
	if stalled, _ := d.EmptyTurn(w); stalled {
		t.Fatal("stalled with a tool in flight")
	}

	// The tool returns.
	writeLines(t, path, toolResultLine)
	d.Poll()
	d.toolCalls = 0 // the toolCalls reprieve is not what should suppress this
	d.lastGrowth = time.Now().Add(-time.Minute)

	stalled, quiet := d.EmptyTurn(w)
	if !stalled {
		t.Fatal("did NOT stall after the only tool returned and the agent went quiet")
	}
	if quiet < w.Idle {
		t.Fatalf("quiet=%v, want >= %v", quiet, w.Idle)
	}
}

// TestEmptyTurnStillFiresWithNoToolAtAll guards the original purpose: a wedged
// agent that never calls a tool must still be caught.
func TestEmptyTurnStillFiresWithNoToolAtAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(path, time.Minute)

	// Some prose, no tool call, then silence.
	writeLines(t, path, `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"thinking"}]}}`)
	d.Poll()

	w := EmptyTurnWindow{Idle: 10 * time.Millisecond, MinGrowth: 1 << 40}
	d.lastGrowth = time.Now().Add(-time.Minute)

	if stalled, _ := d.EmptyTurn(w); !stalled {
		t.Fatal("a wedged agent with no tool call was NOT detected as stalled")
	}
}

// TestToolsInFlightNeverNegative guards the clamp: New() starts mid-file, so a
// toolResult for a call issued before the detector existed is observable. A
// negative count would mask a genuinely outstanding call.
func TestToolsInFlightNeverNegative(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(path, time.Minute)

	// A result with no matching observed issue.
	writeLines(t, path, toolResultLine)
	d.Poll()

	if d.toolsInFlight < 0 {
		t.Fatalf("toolsInFlight=%d, must never go negative", d.toolsInFlight)
	}

	// And a real in-flight call after the clamp must still register.
	writeLines(t, path, toolUseLine)
	d.Poll()
	if d.toolsInFlight != 1 {
		t.Fatalf("toolsInFlight=%d, want 1", d.toolsInFlight)
	}
}
