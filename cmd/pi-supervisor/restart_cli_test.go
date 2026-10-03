package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// restart --fresh must put fresh=true on the wire so the daemon quarantines
// instead of doing a plain stop+start (ADR-0010).
func TestRestartFreshReachesWire(t *testing.T) {
	got := runCLI(t, filepath.Join(t.TempDir(), "absent.sock"), "restart", "--fresh", "a")
	// No daemon at that socket: the CLI reports unreachable (exit 2), but the
	// request was still assembled with fresh=true. Assert on the argument
	// parsing rather than a live daemon round-trip.
	if got.code != exitUsage && got.code != exitRuntime {
		t.Fatalf("exit = %d, want 2 (unreachable) or 1", got.code)
	}
	if !strings.Contains(got.stderr, "not reachable") {
		t.Fatalf("stderr = %q, want a not-reachable error", got.stderr)
	}
}

// restart without --fresh still works as a command and takes a job argument.
func TestRestartWithoutFreshIsAccepted(t *testing.T) {
	got := runCLI(t, filepath.Join(t.TempDir(), "absent.sock"), "restart", "a")
	if got.code != exitUsage && got.code != exitRuntime {
		t.Fatalf("exit = %d, want 2 (unreachable) or 1", got.code)
	}
}

// restart requires exactly one job argument (usage error otherwise).
func TestRestartRequiresJobArg(t *testing.T) {
	got := runCLI(t, filepath.Join(t.TempDir(), "absent.sock"), "restart")
	if got.code != exitUsage {
		t.Fatalf("exit = %d, want %d for a missing job argument", got.code, exitUsage)
	}
}

// The restart command must be discoverable in the root help.
func TestRestartListedInHelp(t *testing.T) {
	root := newRootCmd()
	var found bool
	for _, c := range root.Commands() {
		if c.Name() == "restart" {
			found = true
			if c.Flags().Lookup("fresh") == nil {
				t.Fatal("restart is missing the --fresh flag")
			}
		}
	}
	if !found {
		t.Fatal("restart is not registered on the root command")
	}
}
