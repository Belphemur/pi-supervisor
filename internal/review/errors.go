// Package review implements the daemon side of ADR-0012: the GitHub review
// surface a pi round drives through the `_pi-supervisor-review` shim.
//
// The daemon serves reads and records writes; it never triages and never
// authors a reply body (ADR-0012 §1). What it does own is the budget, the
// answer-before-resolve guard, and the ack gate on bulk close.
package review

import (
	"errors"
	"fmt"
	"net/http"

	"pi-supervisor/internal/fault"
)

// Reason is the closed set of refusal codes the review surface returns
// (ADR-0012 §2.4). The consumer is an LLM, so it must be able to branch on a
// stable symbol instead of parsing prose: a closed enum can be
// exhaustively checked and unit-tested, a sentence cannot.
//
// It is an ALIAS of fault.Kind, not a second enum (ADR-0013 follow-up 1). Both
// reach the client in the same field — `Response.Reason` — and two closed
// enums for one field mean two spellings of the same condition, which an LLM
// shim matches on one of. fault imports nothing, so review importing it
// keeps the dependency graph acyclic; ExitCode() rides along as a method on
// the alias, and the strings are unchanged, so every published reason
// (ADR-0012 §2.4, doc/skill/pi_supervisor_review/SKILL.md) still arrives
// byte-identical.
type Reason = fault.Kind

const (
	// ReasonNoLiveRound: no `review <job>` round is live, so the shim is not
	// inside the round it was armed for. Retrying cannot help. Shared with
	// the steer path — same condition, same symbol.
	ReasonNoLiveRound = fault.KindNoLiveRound
	// ReasonRoundMismatch: the request named a round other than the daemon's
	// live one. Refused, never silently corrected (ADR-0012 §8, Q6).
	ReasonRoundMismatch = fault.KindRoundMismatch
	// ReasonNotAnswered: resolve without a same-round reply. The whole point
	// of the guard: a bare close discards why the finding was handled.
	ReasonNotAnswered = fault.KindNotAnswered
	// ReasonUnknownThread: the thread id is not a PRRT_ node id on this PR.
	ReasonUnknownThread = fault.KindUnknownThread
	// ReasonAuthUnavailable: no usable GitHub credential. Operator action.
	ReasonAuthUnavailable = fault.KindAuthUnavailable
	// ReasonRateLimited: GitHub throttled us. Retry with backoff.
	ReasonRateLimited = fault.KindRateLimited
	// ReasonGitHubError: anything else from the API. The message carries the
	// typed GitHub error.
	ReasonGitHubError = fault.KindGitHubError
	// ReasonUsage: the request itself is malformed (missing pr, bad id shape).
	ReasonUsage = fault.KindUsage
)

// Refusal is a review-surface error carrying a machine-readable reason. It is
// the only error type that crosses the control socket for a review action.
type Refusal struct {
	Reason  Reason `json:"reason"`
	Message string `json:"message,omitempty"`
}

func (e *Refusal) Error() string {
	if e.Message == "" {
		return string(e.Reason)
	}
	return string(e.Reason) + ": " + e.Message
}

// ExitCode is the process exit this refusal maps to.
func (e *Refusal) ExitCode() int { return e.Reason.ExitCode() }

func refuse(r Reason, format string, args ...any) *Refusal {
	return &Refusal{Reason: r, Message: fmt.Sprintf(format, args...)}
}

// Constructors for the refusals the control layer raises. They live here (not
// at the call site) so the reason strings are defined exactly once and an
// exhaustive grep of Reason proves the closed set is what ships.

// ErrUsage: the request itself is malformed. The caller changes the call.
func ErrUsage(format string, args ...any) *Refusal {
	return refuse(ReasonUsage, format, args...)
}

// ErrNoLiveRound: no review round is live for this job, so the shim is not
// inside the round it was armed for. Retrying cannot help.
func ErrNoLiveRound(job string) *Refusal {
	return refuse(ReasonNoLiveRound, "no live review round for job %q; the shim is only valid inside its own round", job)
}

// ErrUnknownJob: the named job is not loaded.
func ErrUnknownJob(job string) *Refusal {
	return refuse(ReasonUsage, "unknown job %q", job)
}

// ErrRoundMismatch: the request named a round other than the daemon's live
// one. Refused, never silently corrected — a stale shim must fail loudly
// rather than act against the wrong round's bookkeeping.
func ErrRoundMismatch(got, want int) *Refusal {
	return refuse(ReasonRoundMismatch, "request named round %d but the live round is %d", got, want)
}

// ErrNotAnswered: resolve without a same-round reply. This is the guard the
// ADR exists to enforce.
func ErrNotAnswered(threadID string, round int) *Refusal {
	return refuse(ReasonNotAnswered,
		"thread %s was not answered in round %d; post_replies first — the reply body is the record of why", threadID, round)
}

// ErrUnknownThread: the id is not a PRRT_ review-thread node id.
func ErrUnknownThread(id string) *Refusal {
	return refuse(ReasonUnknownThread,
		"thread_id %q is not a GraphQL review-thread id (expected %s…); the numeric pulls-comments id is a COMMENT node", id, threadIDPrefix)
}

// classifyGH turns a GitHub HTTP error into the right refusal. Rate limiting
// and auth failures are distinguished because they demand opposite responses
// from an agent: back off, versus stop and ask a human.
func classifyGH(err error) *Refusal {
	if err == nil {
		return nil
	}
	if ghErr, ok := errors.AsType[*GHError](err); ok {
		switch {
		case ghErr.Status == http.StatusForbidden && ghErr.RateLimited:
			return refuse(ReasonRateLimited, "github rate limited: %s", ghErr.Message)
		case ghErr.Status == http.StatusUnauthorized,
			ghErr.Status == http.StatusForbidden && !ghErr.RateLimited:
			return refuse(ReasonAuthUnavailable, "github rejected the credential: %s", ghErr.Message)
		}
		return refuse(ReasonGitHubError, "%s", ghErr.Message)
	}
	return refuse(ReasonGitHubError, "%v", err)
}

// GHError is a GitHub API failure carrying the HTTP status the daemon saw.
type GHError struct {
	Status      int
	Message     string
	RateLimited bool
}

func (e *GHError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("github api status %d", e.Status)
	}
	return fmt.Sprintf("github api status %d: %s", e.Status, e.Message)
}
