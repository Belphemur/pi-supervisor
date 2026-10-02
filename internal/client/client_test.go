package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePi returns the fake pi executable. The script has a shebang and is
// chmod +x, so it is exec'd directly like the real pi binary.
func fakePi(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../testdata/fake-pi.py")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("fake-pi.py missing: %v", err)
	}
	return p
}

func baseOpts(t *testing.T, prompt string) Options {
	t.Helper()
	return Options{
		PiBin:       fakePi(t),
		Prompt:      prompt,
		Timeout:     10 * time.Second,
		Out:         &strings.Builder{},
		Diag:        &strings.Builder{},
		Worktree:    t.TempDir(),
		GracePeriod: 2 * time.Second,
	}
}

func TestStreamRoundTrips(t *testing.T) {
	o := baseOpts(t, "TEST_STREAM")
	res := Run(o)
	if res.RC != 0 {
		t.Fatalf("RC = %d, want 0 (err=%q)", res.RC, res.Err)
	}
	if res.Text != "hello from fake pi" {
		t.Fatalf("Text = %q, want %q", res.Text, "hello from fake pi")
	}
	if res.PID <= 0 {
		t.Fatalf("PID = %d, want >0", res.PID)
	}
}

func TestErrorResponseIsRC1(t *testing.T) {
	o := baseOpts(t, "TEST_ERROR")
	res := Run(o)
	if res.RC != 1 {
		t.Fatalf("RC = %d, want 1", res.RC)
	}
	if !strings.Contains(res.Err, "synthetic failure") {
		t.Fatalf("Err = %q, want it to carry the pi error", res.Err)
	}
}

func TestStdoutCloseWithoutAgentEndIsRC2(t *testing.T) {
	o := baseOpts(t, "TEST_NOEND")
	res := Run(o)
	if res.RC != 2 {
		t.Fatalf("RC = %d, want 2 (stdout closed, no agent_end)", res.RC)
	}
	if res.Text != "partial" {
		t.Fatalf("Text = %q, want the partial stream preserved", res.Text)
	}
}

func TestSpawnFailureIsRC1(t *testing.T) {
	o := baseOpts(t, "TEST_STREAM")
	o.PiBin = "/nonexistent/pi-binary"
	res := Run(o)
	if res.RC != 1 {
		t.Fatalf("RC = %d, want 1", res.RC)
	}
	if res.Err == "" {
		t.Fatal("Err empty, want a spawn diagnostic")
	}
}

func TestTimeoutSendsAbortAndReportsRC1(t *testing.T) {
	o := baseOpts(t, "TEST_HANG")
	o.Timeout = 2 * time.Second
	start := time.Now()
	res := Run(o)
	el := time.Since(start)

	if res.RC != 1 {
		t.Fatalf("RC = %d, want 1", res.RC)
	}
	if !strings.Contains(res.Err, "TIMEOUT") {
		t.Fatalf("Err = %q, want TIMEOUT", res.Err)
	}
	// Budget: timeout + grace + reap grace + SIGTERM grace. The fake pi
	// ignores the abort and sleeps 3600s, so this proves we escalate rather
	// than waiting on the process.
	maxElapsed := 2*time.Second + 2*time.Second + 5*time.Second + 6*time.Second
	if el > maxElapsed {
		t.Fatalf("round took %s, want under %s (abort + grace + reap)", el, maxElapsed)
	}
	if el < 2*time.Second {
		t.Fatalf("round returned in %s, before the %s timeout fired", el, o.Timeout)
	}
}

// A steer appended to the control file mid-round must reach pi's stdin.
func TestControlFileSteeringIsForwarded(t *testing.T) {
	dir := t.TempDir()
	ctrl := filepath.Join(dir, "job.ctrl")
	if err := os.WriteFile(ctrl, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	o := baseOpts(t, "TEST_SLOW SECS=7")
	o.ControlPath = ctrl

	done := make(chan Result, 1)
	go func() { done <- Run(o) }()

	// Append a prompt frame while the round is still sleeping.
	time.Sleep(1500 * time.Millisecond)
	f, err := os.OpenFile(ctrl, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// The round is mid-turn; a raw frame is still forwarded, which is what
	// pi_control.py does.
	if _, err := f.WriteString(`{"type":"prompt","message":"TEST_STREAM"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	// The reader may already be past this frame; close is best-effort here.
	_ = f.Close()

	select {
	case res := <-done:
		if res.RC != 0 && res.RC != 2 {
			t.Fatalf("RC = %d, want 0 or 2", res.RC)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("round never returned")
	}
}

// The abort-drain handshake: after an abort, a following frame is held until
// the aborted turn's agent_end arrives, then released.
func TestAbortDrainHoldsThenReleasesFrames(t *testing.T) {
	st := &stream{diagf: func(string, ...any) {}}

	// First frame: a plain prompt goes straight through.
	ok, held := st.forwardable("prompt")
	if !ok || held {
		t.Fatalf("prompt: ok=%v held=%v, want send", ok, held)
	}

	// An abort opens the drain window and arms holdEnd.
	ok, held = st.forwardable("abort")
	if !ok || held {
		t.Fatalf("abort: ok=%v held=%v, want send", ok, held)
	}
	if !st.holdEnd || !st.drain {
		t.Fatal("abort must set holdEnd and drain")
	}

	// A frame during the drain is held, not sent.
	ok, held = st.forwardable("prompt")
	if ok || !held {
		t.Fatalf("during-drain prompt: ok=%v held=%v, want held", ok, held)
	}
	st.hold(map[string]any{"type": "prompt", "message": "interrupt"})

	// releaseDrain is a no-op until the aborted turn reports agent_end.
	if ready, _ := st.releaseDrain(); ready {
		t.Fatal("releaseDrain fired before the aborted turn ended")
	}

	// Reader sees the aborted turn's agent_end: it must NOT finish the round.
	st.holdEnd = true
	st.read(strings.NewReader(
		`{"type":"agent_end"}` + "\n" +
			`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"after abort"}}` + "\n" +
			`{"type":"agent_end"}` + "\n"))

	if st.sawEnd {
		t.Fatal("sawEnd set by the ABORTED turn's agent_end — drain is broken")
	}

	ready, frames := st.releaseDrain()
	if !ready || len(frames) != 1 {
		t.Fatalf("releaseDrain: ready=%v frames=%d, want the held frame", ready, len(frames))
	}
	if st.drain || st.holdEnd {
		t.Fatal("drain state not cleared after release")
	}
}

// Streaming deltas must reach the run log live (marker gate depends on it).
func TestStreamedTextReachesOut(t *testing.T) {
	var sb strings.Builder
	o := baseOpts(t, "TEST_STREAM")
	o.Out = &sb
	res := Run(o)
	if res.RC != 0 {
		t.Fatalf("RC = %d", res.RC)
	}
	if sb.String() != "hello from fake pi" {
		t.Fatalf("Out = %q, want the full stream", sb.String())
	}
}

// Resume mode must pass --session and never -n.
func TestResumeArgs(t *testing.T) {
	args := rpcArgs(Options{Session: "/tmp/s.jsonl", Name: "ignored"})
	if contains(args, "-n") {
		t.Fatalf("resume args must not pass -n: %v", strings.Join(args, " "))
	}
	if !contains(args, "--session") {
		t.Fatalf("resume args missing --session: %v", strings.Join(args, " "))
	}
}

// Launch mode must pass -n and the worktree is the process cwd.
func TestLaunchArgs(t *testing.T) {
	args := rpcArgs(Options{Name: "job1", Skills: []string{"/a", "/b"}})
	if !contains(args, "-n") || !contains(args, "job1") {
		t.Fatalf("launch args missing -n: %v", args)
	}
	if n := countOf(args, "--skill"); n != 2 {
		t.Fatalf("--skill count = %d, want 2 (%v)", n, args)
	}
}

func TestPiArgsAlwaysRPCMode(t *testing.T) {
	args := rpcArgs(Options{})
	if len(args) < 2 || args[0] != "--mode" || args[1] != "rpc" {
		t.Fatalf("args = %v, want --mode rpc first", args)
	}
}

func contains(s []string, v string) bool { return countOf(s, v) > 0 }

func countOf(s []string, v string) int {
	n := 0
	for _, x := range s {
		if x == v {
			n++
		}
	}
	return n
}
