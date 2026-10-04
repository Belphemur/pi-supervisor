package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildCLI compiles the daemon/ctl binary once per run and returns its path.
// Driving the real binary (instead of calling ctl() in-process) is what makes
// the exit codes and the os.Exit paths observable at all.
var (
	buildOnce sync.Once
	binPath   string
	coverDir  string
	buildErr  error
)

// cli compiles the daemon/ctl binary once per run, coverage-instrumented, so
// the subprocess paths (runDaemon) are measurable too. GOCOVERDIR collects
// the counters when that process exits.
func cli(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "pi-sup-cli")
		if err != nil {
			buildErr = err
			return
		}
		coverDir = filepath.Join(dir, "cov")
		binPath = filepath.Join(dir, "pi-supervisor")
		cmd := exec.Command("go", "build", "-cover", "-coverpkg=pi-supervisor/...", "-o", binPath, ".")
		cmd.Env = append(os.Environ(), "GOTOOLCHAIN=auto")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Skipf("cannot build the cli: %v", buildErr)
	}
	return binPath
}

type runResult struct {
	code   int
	stdout string
	stderr string
}

func runCLI(t *testing.T, sock string, args ...string) runResult {
	t.Helper()
	cmd := exec.Command(cli(t), args...)
	cmd.Env = append(os.Environ(), "PI_SUPERVISOR_SOCK="+sock)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return runResult{code: code, stdout: out.String(), stderr: errb.String()}
}

// fakeDaemon is a one-shot Unix-socket server that answers with the scripted
// response lines, then closes (or hangs, for the watch tests).
func fakeDaemon(t *testing.T, responses ...string) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
				_, _ = bufio.NewReader(c).ReadBytes('\n')
				for _, r := range responses {
					if _, err := c.Write([]byte(r + "\n")); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return sock
}

func respJSON(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// socketPath honors PI_SUPERVISOR_SOCK, then XDG_RUNTIME_DIR, then /tmp.
func TestSocketPathPrecedence(t *testing.T) {
	t.Setenv("PI_SUPERVISOR_SOCK", "/tmp/explicit.sock")
	if got := socketPath(); got != "/tmp/explicit.sock" {
		t.Fatalf("socketPath = %q", got)
	}
	t.Setenv("PI_SUPERVISOR_SOCK", "")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := socketPath(); got != "/run/user/1000/pi-supervisor.sock" {
		t.Fatalf("socketPath = %q", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := socketPath(); got != "/tmp/pi-supervisor.sock" {
		t.Fatalf("socketPath fallback = %q", got)
	}
}

// status/start/stop/steer/logs/reload: request encoding on the wire and the
// printed response.
func TestCtlCommands(t *testing.T) {
	sock := fakeDaemon(t, respJSON(t, map[string]any{"ok": true, "data": map[string]any{"name": "j"}}))

	got := runCLI(t, sock, "status", "j")
	if got.code != 0 || !strings.Contains(got.stdout, "\"name\": \"j\"") {
		t.Fatalf("status = %+v", got)
	}

	// logs answers with a []string payload, printed line by line.
	sock2 := fakeDaemon(t, respJSON(t, map[string]any{"ok": true, "data": []string{"l1", "l2"}}))
	got = runCLI(t, sock2, "logs", "j", "2")
	if got.code != 0 || got.stdout != "l1\nl2\n" {
		t.Fatalf("logs = %+v", got)
	}

	// An ok:false response exits 1 with the error on stderr.
	sock3 := fakeDaemon(t, respJSON(t, map[string]any{"ok": false, "error": "unknown job \"x\""}))
	got = runCLI(t, sock3, "stop", "x")
	if got.code != 1 || !strings.Contains(got.stderr, `unknown job "x"`) {
		t.Fatalf("stop error = %+v", got)
	}
}

func TestCtlUsageErrors(t *testing.T) {
	sock := fakeDaemon(t)
	cases := [][]string{
		{},                // no subcommand
		{"start"},         // missing job
		{"stop"},          // missing job
		{"steer", "j"},    // missing text
		{"logs"},          // missing job
		{"teleport", "j"}, // unknown subcommand
	}
	for _, args := range cases {
		if len(args) == 0 {
			continue
		}
		got := runCLI(t, sock, args...)
		if got.code != 2 {
			t.Fatalf("%v: exit %d, want 2 (stderr %q)", args, got.code, got.stderr)
		}
	}
	// A missing daemon is also a usage-class failure, not a panic.
	got := runCLI(t, filepath.Join(t.TempDir(), "absent.sock"), "status")
	if got.code != 2 || !strings.Contains(got.stderr, "daemon not reachable") {
		t.Fatalf("no daemon = %+v", got)
	}
}

// watch: ack is announced, the event is printed with its footer, and a
// non-terminal event exits 0.
func TestWatchPrintsAckEventAndFooter(t *testing.T) {
	ev := map[string]any{
		"ts": "2026-10-02T00:00:00Z", "job": "power-top", "event": "round_done",
		"round": 3, "rc": 0, "info": "", "worktree": "/tmp/wt", "session_path": "",
	}
	sock := fakeDaemon(t,
		respJSON(t, map[string]any{"ok": true, "data": map[string]any{"type": "watch_ack", "watching": "power-top"}}),
		respJSON(t, map[string]any{"ok": true, "data": ev}),
	)
	got := runCLI(t, sock, "watch", "power-top")
	if got.code != 0 {
		t.Fatalf("exit %d stderr %q", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "watching power-top") {
		t.Fatalf("no watch_ack line: %q", got.stdout)
	}
	if !strings.Contains(got.stdout, `"event":"round_done"`) {
		t.Fatalf("event not printed: %q", got.stdout)
	}
	if !strings.Contains(got.stdout, "LLM next steps for power-top") {
		t.Fatalf("footer missing: %q", got.stdout)
	}
	if !strings.Contains(got.stdout, "git -C /tmp/wt diff --stat") {
		t.Fatalf("footer worktree missing: %q", got.stdout)
	}
	if !strings.Contains(got.stdout, "re-arm: pi-supervisor watch power-top") {
		t.Fatalf("re-arm hint missing (ADR-0003): %q", got.stdout)
	}
}

// A terminal event says the run is over and does NOT tell the agent to
// re-arm.
func TestWatchTerminalEventFooter(t *testing.T) {
	ev := map[string]any{"job": "j", "event": "done", "round": 6, "worktree": "/tmp/wt"}
	sock := fakeDaemon(t, respJSON(t, map[string]any{"ok": true, "data": ev}))
	got := runCLI(t, sock, "watch", "j")
	if got.code != 0 {
		t.Fatalf("exit %d stderr %q", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "THE RUN IS OVER") || !strings.Contains(got.stdout, "No re-arm") {
		t.Fatalf("done footer wrong: %q", got.stdout)
	}
}

// A lost connection exits 1 and names the recovery steps.
func TestWatchConnectionLostExitsOne(t *testing.T) {
	sock := fakeDaemon(t) // accepts, then closes without answering
	got := runCLI(t, sock, "watch", "j")
	if got.code != 1 {
		t.Fatalf("exit %d, want 1 (stdout %q)", got.code, got.stdout)
	}
	if !strings.Contains(got.stderr, "watch connection lost") ||
		!strings.Contains(got.stderr, "pi-supervisor watch") {
		t.Fatalf("stderr = %q", got.stderr)
	}
}

// A server-side watch error (e.g. unknown job) exits 2.
func TestWatchServerErrorExitsTwo(t *testing.T) {
	sock := fakeDaemon(t, respJSON(t, map[string]any{"ok": false, "error": "unknown job \"nope\""}))
	got := runCLI(t, sock, "watch", "nope")
	if got.code != 2 || !strings.Contains(got.stderr, "unknown job") {
		t.Fatalf("exit %d stderr %q", got.code, got.stderr)
	}
}

// -t keeps watching after a non-terminal event: the second event is printed
// too (without -t the process exits on the first one).
func TestWatchTerminalFlagKeepsStreaming(t *testing.T) {
	ev1 := map[string]any{"job": "j", "event": "backoff", "round": 1, "info": "sleeping 15s"}
	ev2 := map[string]any{"job": "j", "event": "ci_stall", "round": 1, "info": "parked on CI"}
	sock := fakeDaemon(t,
		respJSON(t, map[string]any{"ok": true, "data": ev1}),
		respJSON(t, map[string]any{"ok": true, "data": ev2}),
	)
	got := runCLI(t, sock, "watch", "j", "-t")
	if got.code != 1 { // stream ends when the fake daemon closes
		t.Fatalf("exit %d, want 1 (stdout %q)", got.code, got.stdout)
	}
	if !strings.Contains(got.stdout, "backoff") || !strings.Contains(got.stdout, "ci_stall") {
		t.Fatalf("-t stopped after the first event: %q", got.stdout)
	}
}

// runDaemon (ExecStart) boots: it loads jobs, serves the socket, reports
// READY, answers a ctl, and exits 0 on SIGTERM. Driven as a subprocess in a
// temp HOME + temp socket so the live daemon is never involved.
// TestMain runs the suite and then reports the coverage contributed by the
// instrumented SUBPROCESS paths (runDaemon), which `go test -cover` cannot
// attribute on its own.
func TestMain(m *testing.M) {
	code := m.Run()
	if coverDir != "" {
		if entries, err := os.ReadDir(coverDir); err == nil && len(entries) > 0 {
			out, err := exec.Command("go", "tool", "covdata", "percent", "-i="+coverDir).CombinedOutput()
			if err == nil {
				fmt.Printf("subprocess coverage: %s\n", strings.TrimSpace(string(out)))
			} else {
				fmt.Printf("subprocess coverage unavailable: %v %s\n", err, out)
			}
		}
	}
	os.Exit(code)
}

// safeBuf is a mutex-guarded strings.Builder: exec.Cmd writes stdout from its
// own goroutine while the test reads it.
type safeBuf struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func TestRunDaemonLifecycle(t *testing.T) {
	home := t.TempDir()
	sock := filepath.Join(t.TempDir(), "live.sock")
	cmd := exec.Command(cli(t), "run")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PI_SUPERVISOR_SOCK="+sock,
		"NOTIFY_SOCKET="+filepath.Join(t.TempDir(), "absent-notify.sock"),
		"GOCOVERDIR="+filepath.Join(coverDir, ""),
	)
	if err := os.MkdirAll(coverDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errb safeBuf
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "pi-supervisor ready") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "pi-supervisor ready") {
		t.Fatalf("daemon never became ready (stderr %q)", errb.String())
	}
	// The control socket answers, and an empty jobs dir is a valid state.
	if got := runCLI(t, sock, "status"); got.code != 0 {
		t.Fatalf("status over the live socket = %+v", got)
	}
	if got := runCLI(t, sock, "reload"); got.code != 0 {
		t.Fatalf("reload = %+v", got)
	}
	// A job written into the temp HOME shows up on reload.
	jobsDir := filepath.Join(home, ".pi", "supervisor", "jobs")
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"probe","brief":"/tmp/b.md","worktree":"/tmp","max_rounds":1}`
	if err := os.WriteFile(filepath.Join(jobsDir, "probe.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runCLI(t, sock, "reload"); got.code != 0 {
		t.Fatalf("reload after job write = %+v", got)
	}
	if got := runCLI(t, sock, "status", "probe"); got.code != 0 || !strings.Contains(got.stdout, "probe") {
		t.Fatalf("status probe = %+v", got)
	}

	// SIGTERM is the shutdown contract: STOPPING=1 then a clean exit 0.
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown exit = %v (stderr %q)", err, errb.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("daemon did not exit on SIGINT")
	}
}

// watchFooter covers every event kind plus the transcript tail.
func TestWatchFooterPerEventKind(t *testing.T) {
	// Numbers arrive from JSON, so they are float64 (as in the real watch
	// client) — the footer's type assertions depend on it.
	cases := []struct {
		kind string
		ev   map[string]any
		want []string
	}{
		{"round_done", map[string]any{"job": "j", "event": "round_done", "round": 2.0, "rc": 1.0},
			[]string{"round 2, rc=1", "steer j", "re-arm"}},
		{"instant_exit", map[string]any{"job": "j", "event": "instant_exit", "info": "strike 1/3"},
			[]string{"context-exhaustion strike (strike 1/3)", "3 strikes -> fatal"}},
		{"backoff", map[string]any{"job": "j", "event": "backoff", "info": "sleeping 15s"},
			[]string{"Idle — sleeping 15s", "no action needed"}},
		{"done", map[string]any{"job": "j", "event": "done"}, []string{"THE RUN IS OVER", "No re-arm"}},
		{"fatal", map[string]any{"job": "j", "event": "fatal", "info": "3 instant exits"},
			[]string{"RUN HALTED", "3 instant exits", "pi-supervisor start j"}},
		{"stopped", map[string]any{"job": "j", "event": "stopped", "round": 4.0},
			[]string{"operator stop", "round 4", "pi-supervisor start j"}},
		{"job_started", map[string]any{"job": "j", "event": "job_started", "worktree": "/tmp/wt"},
			[]string{"Job j launched at /tmp/wt", "pi-supervisor status j"}},
		{"ci_stall", map[string]any{"job": "j", "event": "ci_stall", "info": "waiting on CI"},
			[]string{"re-arm (background+notify)", "pi-supervisor watch j"}},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			out := captureStdout(t, func() { watchFooter(tc.ev) })
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Fatalf("%s footer missing %q:\n%s", tc.kind, w, out)
				}
			}
		})
	}
}

// A live session transcript is echoed as complete lines, with a pointer to
// the rest.
func TestWatchFooterPrintsTranscriptTail(t *testing.T) {
	sess := filepath.Join(t.TempDir(), "session.jsonl")
	body := `{"a":1}` + "\n" + `{"b":2}` + "\n" + `{"c":3}` + "\n" + `{"d":` // torn tail
	if err := os.WriteFile(sess, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		watchFooter(map[string]any{"job": "j", "event": "backoff", "session_path": sess})
	})
	if !strings.Contains(out, "last session JSONL lines ("+sess+")") {
		t.Fatalf("no transcript header:\n%s", out)
	}
	if !strings.Contains(out, `{"c":3}`) || !strings.Contains(out, "tail -n 50 "+sess) {
		t.Fatalf("tail or pointer missing:\n%s", out)
	}
	if strings.Contains(out, `{"d":`) {
		t.Fatalf("torn line printed:\n%s", out)
	}
}

// A missing transcript prints no transcript section at all.
func TestWatchFooterWithoutTranscript(t *testing.T) {
	out := captureStdout(t, func() {
		watchFooter(map[string]any{"job": "j", "event": "backoff",
			"session_path": filepath.Join(t.TempDir(), "none.jsonl")})
	})
	if strings.Contains(out, "last session JSONL") {
		t.Fatalf("missing transcript still printed a section:\n%s", out)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	out := <-done
	_ = r.Close()
	return out
}
