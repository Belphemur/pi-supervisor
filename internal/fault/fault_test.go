package fault

import (
	"errors"
	"fmt"
	"testing"
)

// The log's `reason=` must come from the code, never from matching prose, and
// an untagged error must degrade to the catch-all rather than a guess.
func TestKindOf(t *testing.T) {
	base := errors.New("unknown job \"x\"")
	tagged := New(KindUnknownJob, base)

	if got := KindOf(tagged); got != KindUnknownJob {
		t.Fatalf("KindOf(tagged) = %q", got)
	}
	// Wrapping must not lose the kind: supervisors add context with %w.
	if got := KindOf(fmt.Errorf("restart: %w", tagged)); got != KindUnknownJob {
		t.Fatalf("KindOf(wrapped) = %q", got)
	}
	if got := KindOf(base); got != KindRefused {
		t.Fatalf("KindOf(untagged) = %q, want %q", got, KindRefused)
	}
	if got := KindOf(nil); got != "" {
		t.Fatalf("KindOf(nil) = %q, want empty", got)
	}
}

// The CLI keys on Error text and its exit codes are a contract (ADR-0008), so
// tagging an error must not change one character of its message.
func TestNewKeepsMessageAndUnwraps(t *testing.T) {
	base := errors.New(`job "x" already running`)
	e := New(KindAlreadyRunning, base)

	if e.Error() != base.Error() {
		t.Fatalf("Error() = %q, want %q", e.Error(), base.Error())
	}
	if !errors.Is(e, base) {
		t.Fatal("errors.Is must see through the tag")
	}
	if New(KindRefused, nil) != nil {
		t.Fatal("New(nil) must stay nil so callers can wrap unconditionally")
	}
}
