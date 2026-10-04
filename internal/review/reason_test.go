package review

import (
	"encoding/json"
	"testing"

	"pi-supervisor/internal/fault"
)

// ADR-0013 follow-up 1: review.Reason used to be a SECOND closed enum for
// the same wire field (Response.Reason). It is now an alias of fault.Kind.
// If someone reintroduces a distinct type this stops compiling — which is the
// point: the duplicate vocabulary must not be representable, not merely unused.
// Aliasing proofs. These stop compiling if review ever grows its own type
// again, which is the point: the duplicate vocabulary must not be
// representable, not merely unused. They are function calls rather than
// `var _ T = …` declarations because the linter (rightly) asks for the type
// to be omitted there — and omitting it would delete the proof.
var (
	takesFaultKind  = func(fault.Kind) {}
	takesReviewKind = func(Reason) {}
	takesEither     = func(Reason) int { return 0 }
)

func TestReasonIsFaultKind(t *testing.T) {
	takesFaultKind(ReasonUsage)
	takesReviewKind(fault.KindUsage)
	takesEither(fault.KindUsage)

	if ReasonNoLiveRound != fault.KindNoLiveRound || ReasonUsage != fault.KindUsage {
		t.Fatal("review constants must BE the fault constants, not copies")
	}
	exit := func(r Reason) int { return r.ExitCode() }
	if exit(ReasonRoundMismatch) != 2 || exit(ReasonGitHubError) != 1 {
		t.Fatal("ExitCode must ride the alias from fault.Kind")
	}
}

// The review reasons are a published contract (ADR-0012 §2.4 and the shim's
// SKILL.md). These strings must arrive byte-identical.
func TestReviewReasonValuesUnchanged(t *testing.T) {
	for kind, want := range map[Reason]string{
		ReasonNoLiveRound:     "no-live-round",
		ReasonRoundMismatch:   "round-mismatch",
		ReasonNotAnswered:     "not-answered-this-round",
		ReasonUnknownThread:   "unknown-thread",
		ReasonAuthUnavailable: "auth-unavailable",
		ReasonRateLimited:     "rate-limited",
		ReasonGitHubError:     "github-error",
		ReasonUsage:           "usage",
	} {
		if string(kind) != want {
			t.Errorf("reason changed value: got %q, want %q", kind, want)
		}
	}
}

// Every review reason is reachable from fault.All() — i.e. the review
// vocabulary is a SUBSET of the single fault vocabulary, not a parallel list
// that can drift.
func TestReviewReasonsAreInTheFaultVocabulary(t *testing.T) {
	known := make(map[fault.Kind]bool)
	for _, k := range fault.All() {
		known[k] = true
	}
	for _, k := range []Reason{
		ReasonNoLiveRound, ReasonRoundMismatch, ReasonNotAnswered, ReasonUnknownThread,
		ReasonAuthUnavailable, ReasonRateLimited, ReasonGitHubError, ReasonUsage,
	} {
		if !known[k] {
			t.Errorf("review reason %q is not in fault.All()", k)
		}
	}
}

// The refusal still serializes with the wire key `reason` (the JSON shape is a
// contract; control.Response.Reason stays).
func TestRefusalJSONKeepsReasonField(t *testing.T) {
	data, err := json.Marshal(ErrNotAnswered("PRRT_x", 3))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Reason != "not-answered-this-round" {
		t.Fatalf("reason = %q", out.Reason)
	}
	if ErrNotAnswered("PRRT_x", 3).ExitCode() != 2 {
		t.Fatal("exit code must survive the alias")
	}
}
