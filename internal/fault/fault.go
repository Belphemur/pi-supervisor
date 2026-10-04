// Package fault carries a machine-readable refusal kind alongside the human
// error text, so the journal can log `reason=unknown_job` without re-deriving
// it from prose at the socket (ADR-0008 exit codes are a contract; the log's
// reasons are the same idea one layer down).
//
// The supervisor produces these; internal/control consumes them. The
// vocabulary lives here rather than in either package so neither has to
// import the other: control cannot import supervisor (supervisor imports
// control) and supervisor must not depend on the wire layer.
package fault

import "errors"

// Kind is the closed set of refusal symbols that appear as `reason=` in the
// journal. Anything a handler returns without a Kind is logged as
// KindRefused — the log must never claim a more specific cause than the code
// actually proved.
type Kind string

const (
	KindUnknownJob     Kind = "unknown_job"
	KindNotRunning     Kind = "not_running"
	KindNoLiveRound    Kind = "no_live_round"
	KindAlreadyRunning Kind = "already_running"
	KindAlreadyDone    Kind = "already_done"
	KindBadRequest     Kind = "bad_request"
	KindBadJSON        Kind = "bad_json"
	KindUnknownCmd     Kind = "unknown_cmd"
	KindUnsupported    Kind = "unsupported"
	KindRefused        Kind = "refused"
)

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
