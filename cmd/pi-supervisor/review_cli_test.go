package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The review verbs validate their flags BEFORE touching the socket, so these
// run against an absent socket and must still fail usage-class (exit 2).
func TestReviewCmdValidation(t *testing.T) {
	sock := absentSocket(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no pr and no auto", []string{"review", "j"}, "--pr"},
		{"pr and auto together", []string{"review", "j", "--pr", "43", "--auto"}, "mutually exclusive"},
		{"bad type", []string{"review", "j", "--pr", "43", "--type", "nonsense"}, "--type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runCLI(t, sock, c.args...)
			if r.code == 0 {
				t.Fatalf("expected a validation failure, got success: %s", r.stdout)
			}
			if !strings.Contains(r.stderr, c.want) {
				t.Fatalf("stderr %q does not mention %q", r.stderr, c.want)
			}
		})
	}
}

func TestAckCmdRequiresEvent(t *testing.T) {
	sock := absentSocket(t)
	r := runCLI(t, sock, "ack", "j")
	if r.code == 0 {
		t.Fatal("ack without --event must fail")
	}
	if !strings.Contains(r.stderr, "--event") {
		t.Fatalf("stderr %q does not guide toward --event", r.stderr)
	}
}

// The exit class must come from the reason symbol, never from message text:
// that is what lets an LLM decide retry-vs-change-the-call.
func TestReviewFailureExitCodes(t *testing.T) {
	two := []string{"usage", "no-live-round", "round-mismatch",
		"not-answered-this-round", "unknown-thread"}
	for _, r := range two {
		if got := (&reviewFailure{Reason: r}).ExitCode(); got != exitUsage {
			t.Errorf("reason %q exit = %d, want %d", r, got, exitUsage)
		}
	}
	one := []string{"auth-unavailable", "rate-limited", "github-error"}
	for _, r := range one {
		if got := (&reviewFailure{Reason: r}).ExitCode(); got != exitRuntime {
			t.Errorf("reason %q exit = %d, want %d", r, got, exitRuntime)
		}
	}
	// An unknown/empty reason is a runtime refusal, never guessed as usage:
	// telling an agent to change a call that was actually fine is worse than a
	// generic retry signal.
	if got := (&reviewFailure{}).ExitCode(); got != exitRuntime {
		t.Errorf("empty reason exit = %d, want %d", got, exitRuntime)
	}
	if got := (&reviewFailure{Reason: "brand-new-reason"}).ExitCode(); got != exitRuntime {
		t.Errorf("unknown reason exit = %d, want %d", got, exitRuntime)
	}
}

// End to end over a real socket: a campaign verb succeeds and the reply is
// rendered, with --json available for structured consumption.
func TestReviewOverControlSocket(t *testing.T) {
	sock := fakeDaemon(t, `{"ok":true,"data":{"job":"j","pr":43,"rounds":5}}`)
	r := runCLI(t, sock, "review", "j", "--pr", "43")
	if r.code != 0 {
		t.Fatalf("exit = %d, stderr = %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "pr=43") && !strings.Contains(r.stdout, `"pr": 43`) {
		t.Fatalf("stdout %q does not carry the PR", r.stdout)
	}
}

func TestReviewJSONOutputIsMachineReadable(t *testing.T) {
	sock := fakeDaemon(t, `{"ok":true,"data":{"job":"j","pr":43}}`)
	r := runCLI(t, sock, "review", "j", "--pr", "43", "--json")
	if r.code != 0 {
		t.Fatalf("exit = %d, stderr = %s", r.code, r.stderr)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.stdout)), &out); err != nil {
		t.Fatalf("--json did not emit valid JSON: %v\n%s", err, r.stdout)
	}
}

// A refusal must exit with the class its reason implies and print the symbol,
// so the calling agent can branch without reading English.
func TestReviewRefusalExitCodeAndReason(t *testing.T) {
	cases := []struct {
		reason string
		want   int
	}{
		{"not-answered-this-round", exitUsage},
		{"no-live-round", exitUsage},
		{"unknown-thread", exitUsage},
		{"rate-limited", exitRuntime},
		{"auth-unavailable", exitRuntime},
		{"github-error", exitRuntime},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			sock := fakeDaemonShort(t,
				`{"ok":false,"error":"refused","reason":"`+c.reason+`"}`)
			r := runCLI(t, sock, "review-action", "resolve_thread", "--job", "j")
			if r.code != c.want {
				t.Fatalf("reason %s: exit = %d, want %d (stderr %s)",
					c.reason, r.code, c.want, r.stderr)
			}
		})
	}
}

// The commands must exist and document themselves (ADR-0008): every verb has
// --help, and the flags the ADR specifies are present.
func TestReviewCommandsAreWired(t *testing.T) {
	sock := absentSocket(t)
	for _, args := range [][]string{
		{"review", "--help"},
		{"ack", "--help"},
		{"review-action", "--help"},
	} {
		r := runCLI(t, sock, args...)
		if r.code != 0 {
			t.Fatalf("%v --help exited %d: %s", args, r.code, r.stderr)
		}
		if !strings.Contains(r.stdout, "Usage:") {
			t.Errorf("%v --help has no usage section:\n%s", args, r.stdout)
		}
	}
}

// The flags the ADR specifies must be discoverable from help, since the
// consumer is an LLM reading --help rather than a human reading a manual.
func TestReviewHelpDocumentsFlags(t *testing.T) {
	sock := absentSocket(t)
	r := runCLI(t, sock, "review", "--help")
	if r.code != 0 {
		t.Fatalf("exit = %d", r.code)
	}
	for _, f := range []string{"--pr", "--rounds", "--type", "--auto", "--json"} {
		if !strings.Contains(r.stdout, f) {
			t.Errorf("review --help does not document %s", f)
		}
	}
	r = runCLI(t, sock, "ack", "--help")
	if !strings.Contains(r.stdout, "--event") {
		t.Error("ack --help does not document --event")
	}
}

// The shim transport is a real, callable verb — pi reaches GitHub only
// through it, so its absence would break every review round.
func TestReviewActionCmdUsable(t *testing.T) {
	sock := fakeDaemon(t, `{"ok":true,"data":{"open":2,"threads":[]}}`)
	r := runCLI(t, sock, "review-action", "list_threads", "--job", "j", "--pr", "43")
	if r.code != 0 {
		t.Fatalf("exit = %d, stderr = %s", r.code, r.stderr)
	}
}

// fakeDaemonShort is fakeDaemon with a short socket path: a unix sun_path is
// capped near 108 bytes, and t.TempDir() under a long subtest name overflows
// it with a confusing "bind: invalid argument".
func fakeDaemonShort(t *testing.T, responses ...string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ps")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
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

// absentSocket points at a path nothing is listening on, so validation runs
// without a daemon.
func absentSocket(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/absent.sock"
}
