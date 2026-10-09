package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMidTurnSteerCarriesStreamingBehavior: pi's RPC rejects a plain prompt
// frame while the agent is processing ("Agent is already processing"), and
// that rejection used to fail the WHOLE round (flambette#65 campaign rounds
// 7-8). The client must enrich a mid-turn control-file prompt with
// streamingBehavior "steer" so pi interjects it into the live turn instead.
// An idle-agent prompt (the abort-drain release path) keeps the raw shape.
func TestMidTurnSteerCarriesStreamingBehavior(t *testing.T) {
	dir := t.TempDir()
	ctrl := filepath.Join(dir, "job.ctrl")
	if err := os.WriteFile(ctrl, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	frameLog := filepath.Join(dir, "frames.jsonl")

	o := baseOpts(t, "TEST_SLOW SECS=7")
	o.ControlPath = ctrl
	t.Setenv("FAKE_PI_FRAME_LOG", frameLog)

	done := make(chan Result, 1)
	go func() { done <- Run(o) }()

	// Append a prompt frame while the round is still sleeping (mid-turn).
	time.Sleep(1500 * time.Millisecond)
	f, err := os.OpenFile(ctrl, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"steer-1","type":"prompt","message":"TEST_STREAM"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	select {
	case res := <-done:
		if res.RC != 0 && res.RC != 2 {
			t.Fatalf("RC = %d, want 0 or 2", res.RC)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("round never returned")
	}

	raw, err := os.ReadFile(frameLog)
	if err != nil {
		t.Fatalf("fake-pi frame log missing: %v", err)
	}
	var steered map[string]any
	for line := range splitLines(string(raw)) {
		var fr map[string]any
		if json.Unmarshal([]byte(line), &fr) != nil {
			continue
		}
		if fr["id"] == "steer-1" {
			steered = fr
		}
	}
	if steered == nil {
		t.Fatal("the steer frame never reached pi")
	}
	if steered["streamingBehavior"] != "steer" {
		t.Fatalf("mid-turn prompt forwarded without streamingBehavior: %v", steered)
	}
}

func splitLines(s string) func(func(string) bool) {
	return func(yield func(string) bool) {
		start := 0
		for i := 0; i < len(s); i++ {
			if s[i] == '\n' {
				if !yield(s[start:i]) {
					return
				}
				start = i + 1
			}
		}
		if start < len(s) {
			yield(s[start:])
		}
	}
}
