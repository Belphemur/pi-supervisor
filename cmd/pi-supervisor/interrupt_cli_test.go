package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// recordingDaemon captures the request the CLI sent, then answers ok with a
// steer report echoing the interrupt flag. It proves the CLI's --interrupt /
// -i flag reaches the wire as request.interrupt.
func recordingDaemon(t *testing.T, captured chan string) string {
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
				line, _ := bufio.NewReader(c).ReadBytes('\n')
				select {
				case captured <- string(line):
				default:
				}
				resp, _ := json.Marshal(map[string]any{"ok": true, "data": map[string]any{
					"job": "j", "frame_id": "f1", "round": 1, "job_state": "running",
					"live_round": true, "client_pid": 42, "interrupted": true,
					"outcome": "forwarded", "confirmed": true,
				}})
				_, _ = c.Write(append(resp, '\n'))
			}(c)
		}
	}()
	return sock
}

func TestSteerInterruptFlagReachesWire(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"plain steer", []string{"steer", "j", "focus", "on", "tests"}, false},
		{"long flag", []string{"steer", "j", "--interrupt", "stop", "now"}, true},
		{"short flag", []string{"steer", "j", "-i", "stop", "now"}, true},
		{"with no-wait", []string{"steer", "j", "-n", "-i", "stop", "now"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captured := make(chan string, 4)
			sock := recordingDaemon(t, captured)
			got := runCLI(t, sock, tc.args...)
			if got.code != 0 {
				t.Fatalf("exit %d stderr %q", got.code, got.stderr)
			}
			select {
			case raw := <-captured:
				var req map[string]any
				if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &req); err != nil {
					t.Fatalf("request not json: %q", raw)
				}
				if got, _ := req["interrupt"].(bool); got != tc.want {
					t.Fatalf("request.interrupt = %v, want %v (request %q)", got, tc.want, raw)
				}
				// The prose text must never leak the flags.
				if txt, _ := req["text"].(string); strings.Contains(txt, "--interrupt") ||
					strings.Contains(txt, "-i ") || strings.Contains(txt, "-n") {
					t.Fatalf("flag text leaked into steer payload: %q", txt)
				}
			default:
				t.Fatal("daemon captured no request")
			}
			// The human report states that the interrupt was sent.
			if tc.want && !strings.Contains(got.stdout, "interrupt") {
				t.Fatalf("report omits interrupt line: %q", got.stdout)
			}
		})
	}
}

// --interrupt on a job with no live round still reports the failure honestly.
func TestSteerInterruptNoLiveRoundFails(t *testing.T) {
	sock := fakeDaemon(t, respJSON(t, map[string]any{
		"ok": false, "error": "no live round",
		"data": map[string]any{"job": "j", "outcome": "no live round", "interrupted": false},
	}))
	got := runCLI(t, sock, "steer", "j", "-i", "hello")
	if got.code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %q)", got.code, got.stderr)
	}
}
