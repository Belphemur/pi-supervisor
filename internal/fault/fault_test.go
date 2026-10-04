package fault

import (
	"errors"
	"fmt"
	"strings"
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

// The exit-code table must cover EVERY kind. A Kind added to the vocabulary
// without an entry is otherwise invisible: ExitCode's fallback is 1, so the
// omission would silently reclassify a usage error as retryable.
func TestEveryKindHasAnExitCode(t *testing.T) {
	all := All()
	if len(all) != len(exitCodes) {
		t.Fatalf("All() has %d kinds, exitCodes has %d — one was added without the other",
			len(all), len(exitCodes))
	}
	for _, k := range all {
		if _, ok := exitCodes[k]; !ok {
			t.Errorf("kind %q has no exit-code mapping", k)
			continue
		}
		if code := k.ExitCode(); code != 1 && code != 2 {
			t.Errorf("kind %q exit code = %d, want 1 or 2 (ADR-0008)", k, code)
		}
	}
}

// The values are a published contract (ADR-0012 §2.4), so pin them: a shim
// branches on these exact strings.
func TestKindValuesAreStable(t *testing.T) {
	want := map[string]Kind{
		"unknown_job":             KindUnknownJob,
		"not_running":             KindNotRunning,
		"already_running":         KindAlreadyRunning,
		"already_done":            KindAlreadyDone,
		"bad_request":             KindBadRequest,
		"bad_json":                KindBadJSON,
		"unknown_cmd":             KindUnknownCmd,
		"unsupported":             KindUnsupported,
		"refused":                 KindRefused,
		"no-live-round":           KindNoLiveRound,
		"round-mismatch":          KindRoundMismatch,
		"not-answered-this-round": KindNotAnswered,
		"unknown-thread":          KindUnknownThread,
		"auth-unavailable":        KindAuthUnavailable,
		"rate-limited":            KindRateLimited,
		"github-error":            KindGitHubError,
		"usage":                   KindUsage,
	}
	for value, kind := range want {
		if string(kind) != value {
			t.Errorf("kind %q changed value to %q — the reason strings are a wire contract", value, kind)
		}
	}
}

// ADR-0013 follow-up 1: two closed enums for one wire field meant two
// spellings of the same condition, and a shim matching one missed the other
// (`no_live_round` on the steer path vs `no-live-round` on the review path).
// This test FAILS if anyone re-adds a kind whose value is the underscore or
// hyphen twin of a value already in the vocabulary.
func TestNoDuplicateSpellings(t *testing.T) {
	seen := make(map[Kind]bool, len(All()))
	for _, k := range All() {
		if seen[k] {
			t.Fatalf("kind %q appears twice in All()", k)
		}
		seen[k] = true
	}
	twins := func(s string) string {
		out := []byte(s)
		for i, c := range out {
			switch c {
			case '_':
				out[i] = '-'
			case '-':
				out[i] = '_'
			}
		}
		return string(out)
	}
	for _, k := range All() {
		s := string(k)
		if !strings.ContainsAny(s, "-_") {
			continue // a single word is its own twin; nothing to collide with
		}
		if twin := twins(s); seen[Kind(twin)] {
			t.Errorf("kind %q duplicates %q — one condition, one spelling on the wire", k, twin)
		}
	}
}

// `no_live_round` was the steer path's spelling of a condition review had
// already published as `no-live-round`. It must stay retired: KindOf must
// never hand back a spelling no client matches.
func TestLegacyNoLiveRoundSpellingIsRetired(t *testing.T) {
	const legacy Kind = "no_live_round"
	for _, k := range All() {
		if k == legacy {
			t.Fatalf("legacy spelling %q is back in the vocabulary", legacy)
		}
	}
	if got := KindOf(New(KindNoLiveRound, errors.New("no round is reading the ctrl file"))); got != KindNoLiveRound {
		t.Fatalf("KindOf = %q, want %q", got, KindNoLiveRound)
	}
}

// Every reason that reaches the review shim maps onto the CLI contract by
// shape: a call that cannot succeed as written exits 2, a runtime refusal
// exits 1.
func TestUsageLikeKindsAgreeOnExit(t *testing.T) {
	for _, k := range []Kind{KindUsage, KindRoundMismatch, KindUnknownThread,
		KindNoLiveRound, KindNotAnswered} {
		if k.ExitCode() != 2 {
			t.Errorf("%q exit code = %d, want 2 (change the call)", k, k.ExitCode())
		}
	}
	for _, k := range []Kind{KindAuthUnavailable, KindRateLimited, KindGitHubError, KindRefused} {
		if k.ExitCode() != 1 {
			t.Errorf("%q exit code = %d, want 1 (retry)", k, k.ExitCode())
		}
	}
	// The control-layer kinds stay 1 on purpose: their CLI exits come from
	// prose (ADR-0008 predates Reason), so 2 here would invent a usage exit
	// nobody implemented. Pin it so the choice cannot drift unnoticed.
	for _, k := range []Kind{KindBadRequest, KindBadJSON, KindUnknownCmd,
		KindUnknownJob, KindNotRunning, KindAlreadyRunning, KindAlreadyDone,
		KindUnsupported} {
		if k.ExitCode() != 1 {
			t.Errorf("%q exit code = %d, want 1 (control verbs exit off prose)", k, k.ExitCode())
		}
	}
	if KindUsage == KindBadRequest {
		t.Fatal("usage and bad_request are distinct published values; do not collapse them")
	}
	// An unknown or empty reason is a runtime refusal, never a guessed usage
	// error — the same rule the review CLI applies before it converts.
	if Kind("").ExitCode() != 1 || Kind("no-such-reason").ExitCode() != 1 {
		t.Fatal("unknown reasons must exit 1")
	}
}
