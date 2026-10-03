package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// version command is offline (no socket): it prints the build-time version.
// When built via `go test` without -ldflags, version.Version() returns "dev".
func TestVersionCommand(t *testing.T) {
	got := runCLI(t, filepath.Join(t.TempDir(), "absent.sock"), "version")
	if got.code != 0 {
		t.Fatalf("exit %d stderr %q", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "pi-supervisor") {
		t.Fatalf("output missing binary name: %q", got.stdout)
	}
	if !strings.Contains(got.stdout, "dev") {
		t.Fatalf("expected 'dev' for a non-linked build, got: %q", got.stdout)
	}
}
