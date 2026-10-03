package version

import "testing"

func TestVersionDefault(t *testing.T) {
	// Without -X injection, ver is "dev" (see version.go). A release build
	// overrides this via -ldflags=-X; this test locks the no-injection default.
	if got := Version(); got != "dev" {
		t.Fatalf("Version() = %q, want %q", got, "dev")
	}
}

func TestVersionOverridable(t *testing.T) {
	// Simulate a linked-in release version by setting the package var directly.
	// (ldflags -X sets this at link time, but in-process we assign to prove the
	// plumbing works.)
	orig := ver
	t.Cleanup(func() { ver = orig })
	ver = "v1.2.3-abc123"
	if got := Version(); got != "v1.2.3-abc123" {
		t.Fatalf("Version() = %q, want %q", got, "v1.2.3-abc123")
	}
}
