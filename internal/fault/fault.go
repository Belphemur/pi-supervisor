// Package fault carries a machine-readable refusal kind alongside the human
// error text, so the journal can log `reason=unknown_job` without re-deriving
// it from prose at the socket (ADR-0008 exit codes are a contract; the log's
// reasons are the same idea one layer down).
//
// The supervisor produces these; internal/control consumes them, and
// internal/review aliases them (see review.Reason). The vocabulary lives here
// rather than in either package so neither has to import the other: control
// cannot import supervisor (supervisor imports control) and supervisor must
// not depend on the wire layer. review can import fault (fault imports
// nothing), so review.Reason is an ALIAS of Kind rather than a second enum.
//
// ONE vocabulary, ONE spelling per condition (ADR-0013 follow-up 1). Two
// closed enums used to describe the same wire field — `Response.Reason` —
// with underscore spellings here and hyphen spellings in review, so a shim
// matching one silently missed the other. The published values are a contract
// (ADR-0012 §2.4, doc/skill/pi_supervisor_review/SKILL.md), so the HYPHEN
// spellings won: this package's own `no_live_round` was retired to match
// review's already-published `no-live-round`.
package fault

import "errors"

// Kind is the closed set of refusal symbols that appear as `reason=` in the
// journal AND as `reason` on the control socket (ADR-0012 §2.4). Anything a
// handler returns without a Kind is logged as KindRefused — the log must
// never claim a more specific cause than the code actually proved.
type Kind string

const (
	// Control-layer refusals (the daemon verbs). Underscore spellings: these
	// were never published to the review shim.
	KindUnknownJob     Kind = "unknown_job"
	KindNotRunning     Kind = "not_running"
	KindAlreadyRunning Kind = "already_running"
	KindAlreadyDone    Kind = "already_done"
	KindBadRequest     Kind = "bad_request"
	KindBadJSON        Kind = "bad_json"
	KindUnknownCmd     Kind = "unknown_cmd"
	KindUnsupported    Kind = "unsupported"
	KindRefused        Kind = "refused"

	// NoLiveRound: no round of THIS job is live. Shared by steer (no round
	// is reading the ctrl file) and by the review surface (the shim is not
	// inside the round it was armed for). One condition, one spelling: the
	// hyphen form is the one ADR-0012 §2.4 published, so it is the one both
	// paths emit.
	KindNoLiveRound Kind = "no-live-round"

	// Review-surface refusals (ADR-0012 §2.4). Values are a published
	// contract — the shim branches on these strings.
	KindRoundMismatch   Kind = "round-mismatch"
	KindNotAnswered     Kind = "not-answered-this-round"
	KindUnknownThread   Kind = "unknown-thread"
	KindAuthUnavailable Kind = "auth-unavailable"
	KindRateLimited     Kind = "rate-limited"
	KindGitHubError     Kind = "github-error"
	// KindUsage: the CALL is malformed (missing pr, bad id shape, unusable
	// verb). It is the review-facing sibling of KindBadRequest — same idea
	// ("change the call"), different surface, both exit 2. The values stay
	// distinct because both are published; fault_test.go pins the relation.
	KindUsage Kind = "usage"

	// Task-watch lookup failures (ADR-0014 §5). A completed-looking TaskUpdate
	// execution that the plugin's JSON cannot confirm gets ONE diagnostic with
	// one of these reasons. They are wire diagnostics, not CLI refusals, but
	// they live in this vocabulary — one spelling per condition, exit codes
	// from the same table as everything else.
	KindTaskStoreMissing Kind = "task-store-missing"
	KindTaskStoreMemory  Kind = "task-store-memory"
	KindTaskStoreInvalid Kind = "task-store-invalid"
	KindTaskMissing      Kind = "task-missing"
	KindTaskAmbiguous    Kind = "task-ambiguous"
	KindTaskNotCompleted Kind = "task-not-completed"
	KindTaskBadInput     Kind = "task-bad-input"
	KindTaskNoIdentity   Kind = "task-identity-unavailable"
)

// All is every Kind in the vocabulary, in declaration order. Tests use it to
// prove the exit-code table and the enum cannot drift apart; production code
// does not iterate it.
func All() []Kind {
	return []Kind{
		KindUnknownJob, KindNotRunning, KindAlreadyRunning, KindAlreadyDone,
		KindBadRequest, KindBadJSON, KindUnknownCmd, KindUnsupported, KindRefused,
		KindNoLiveRound, KindRoundMismatch, KindNotAnswered, KindUnknownThread,
		KindAuthUnavailable, KindRateLimited, KindGitHubError, KindUsage,
	}
}

// exitCodes is the whole of the ADR-0008 exit contract as data, so a Kind
// without an entry is a compile-and-test-visible gap rather than a silent
// fallthrough. 2 = usage/validation, the caller must change the call; 1 =
// refused at runtime, the caller may retry.
//
// The control-layer kinds are all 1 on purpose: their CLI exits come from
// prose (ADR-0008 predates Reason), so mapping them here to 2 would invent a
// usage exit nobody implemented.
var exitCodes = map[Kind]int{
	KindNoLiveRound:     2,
	KindRoundMismatch:   2,
	KindNotAnswered:     2,
	KindUnknownThread:   2,
	KindUsage:           2,
	KindBadRequest:      1,
	KindBadJSON:         1,
	KindUnknownCmd:      1,
	KindUnsupported:     1,
	KindUnknownJob:      1,
	KindNotRunning:      1,
	KindAlreadyRunning:  1,
	KindAlreadyDone:     1,
	KindRefused:         1,
	KindAuthUnavailable: 1,
	KindRateLimited:     1,
	KindGitHubError:     1,

	KindTaskStoreMissing: 1,
	KindTaskStoreMemory:  1,
	KindTaskStoreInvalid: 1,
	KindTaskMissing:      1,
	KindTaskAmbiguous:    1,
	KindTaskNotCompleted: 1,
	KindTaskBadInput:     2,
	KindTaskNoIdentity:   1,
}

// ExitCode maps a reason onto the CLI's exit contract (ADR-0008):
// 2 = usage/validation — the caller must change the call; 1 = refused at
// runtime — the caller may retry. Keeping this on the reason (not on the
// message) is what lets a shim branch without reading English. An unknown or
// empty reason is a runtime refusal (1), never a guess at "usage".
func (k Kind) ExitCode() int {
	if code, ok := exitCodes[k]; ok {
		return code
	}
	return 1
}

// Error pairs a Kind with the underlying cause. Error() returns the cause's
// text unchanged: the CLI's exit-code contract keys on that prose (ADR-0008),
// and this type adds metadata without changing any existing message.
type Error struct {
	Kind Kind
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// New tags err with a Kind. It returns the error unchanged when err is nil so
// callers can wrap unconditionally.
func New(kind Kind, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Kind: kind, Err: err}
}

// KindOf reports the refusal kind carried by err, walking the wrap chain.
// An untagged error yields KindRefused — never a guess.
func KindOf(err error) Kind {
	if fe, ok := errors.AsType[*Error](err); ok {
		return fe.Kind
	}
	if err == nil {
		return ""
	}
	return KindRefused
}
