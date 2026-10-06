package supervisor

import (
	"context"
	"fmt"
	"time"

	"pi-supervisor/internal/review"
)

// reviewReader is the GitHub read surface the campaign gate consumes. An
// interface so tests can stub GitHub without a network; production satisfies
// it with *review.Client (method-for-method).
type reviewReader interface {
	ListThreads(ctx context.Context, owner, repo string, pr int) ([]review.Thread, error)
	CheckCI(ctx context.Context, owner, repo string, pr int) (*review.CIRollup, error)
}

// gateReader acquires the gate's GitHub client. A package var mirroring
// listOpenThreads so tests can stub it.
var gateReader = func(s *Supervisor, ctx context.Context, owner, repo string) (reviewReader, error) {
	return s.reviewClient(ctx, owner, repo)
}

// reviewGate is the campaign's exit condition, evaluated between rounds
// (ADR-0012 §4.2): `0 open threads && CI pass`.
//
// It replaces the old trailing `pre-merge --pr N`. That gate's wall-clock
// `wait` is now MaxRounds and its thread/CI reads happen here instead, so
// re-deriving them on demand was redundant state the daemon already holds.
//
// Returns true when the campaign is closed and the caller must stop looping.
//
// The checks are INDEPENDENT on purpose: a campaign with threads still open is
// not done even if CI is green, and a campaign whose CI is red is not done even
// with zero threads. Reporting only the blocking condition matters, because
// this message is what the LLM reads to decide whether to re-arm.
func (s *Supervisor) reviewGate(r *runner, round int) bool {
	r.mu.Lock()
	camp := r.review
	name := r.job.Name
	wt := r.job.Worktree
	r.mu.Unlock()
	if camp == nil {
		return false
	}
	camp.mu.Lock()
	owner, repo, pr := camp.owner, camp.repo, camp.pr
	kind, used := camp.kind, camp.round
	camp.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	gh, err := gateReader(s, ctx, owner, repo)
	if err != nil {
		// A credential problem is NOT the campaign's verdict: leave it
		// running and let the operator fix auth. Failing here would end a
		// healthy campaign on a transient outage.
		s.emit(name, "review_gate_error", round, 0, 0, "",
			"could not read review state (%v); campaign continues, re-run `pi-supervisor review %s --pr %d` if this repeats",
			err, name, pr)
		return false
	}

	all, err := gh.ListThreads(ctx, owner, repo, pr)
	if err != nil {
		s.emit(name, "review_gate_error", round, 0, 0, "",
			"could not list threads (%v); campaign continues", err)
		return false
	}
	open := review.OpenThreads(all)
	roll, err := gh.CheckCI(ctx, owner, repo, pr)
	ciVerdict := "unknown"
	if err == nil && roll != nil {
		ciVerdict = roll.Verdict
	}

	// Report the state every round, so `watch` is a real dashboard rather
	// than a silent loop (ADR-0012 §7). A CI FAILURE names the required
	// checks that are red: "ci=fail" alone tells an LLM nothing actionable,
	// and only required checks gate the campaign.
	ciNote := ""
	if roll != nil && roll.Verdict == "fail" {
		ciNote = fmt.Sprintf(" — required: %s", roll.BlockingSummary())
		if nb := roll.NonBlockingSummary(); nb != "" {
			ciNote += fmt.Sprintf(" (non-blocking, ignored: %s)", nb)
		}
	}
	// Record the observed open count on the campaign so an exhausted close can
	// record a truthful baseline (see reviewCampaign.lastOpen).
	camp.setLastOpen(len(open))
	s.emit(name, "review_round_done", round, 0, 0, "",
		"review round %d/%d (%s): %d open thread(s), ci=%s%s on %s/%s#%d",
		used, camp.maxRound, kind, len(open), ciVerdict, ciNote, owner, repo, pr)

	if len(open) == 0 && ciVerdict == "pass" {
		r.mu.Lock()
		r.state.State, r.active = "done", false
		r.state.LastDiag = "review campaign clean: 0 open threads, CI passing"
		// The campaign is spent. Leaving it armed would let a later `start`
		// re-enter `reviewing` with a stale budget (ADR-0016): the terminal
		// state must be reachable as done again.
		r.review = nil
		r.mu.Unlock()
		r.persistState()
		s.logf(name, "review campaign complete after %d round(s): 0 open threads, CI passing", used)
		s.emit(name, "review_done", round, 0, 0, "",
			"review campaign complete: 0 open threads and CI passing on %s/%s#%d after %d round(s) — the owner merges, the daemon never does",
			owner, repo, pr, used)
		// Record the baseline so a LATER push that attracts new findings is
		// detectable. `len(open)` is 0 here, but the campaign may have ended on
		// review_exhausted with threads still open, so record whatever the last
		// observed count was rather than assuming zero.
		s.recordThreadBaseline(name, r, owner, repo, pr, len(open))
		// The job is idle and a baseline now exists, so this is the one point
		// the re-check can actually run. Firing it only from the build job's
		// marker-done branch would mean it never sees a baseline at all:
		// baselines are created HERE and in the exhausted path, both of which
		// leave the job terminal and refused by Start.
		go s.recheckThreads(name)
		_ = wt
		return true
	}

	// Not done. Name the ONE blocking condition so the message is actionable:
	// both open threads and pending CI are normal mid-campaign, and saying
	// "not done" without saying why is what makes an LLM re-arm blindly.
	switch {
	case len(open) > 0 && ciVerdict == "fail":
		s.logf(name, "review round %d: %d thread(s) open, required CI red: %s",
			round, len(open), roll.BlockingSummary())
	case len(open) > 0 && ciVerdict != "pass":
		s.logf(name, "review round %d: %d thread(s) open, ci=%s", round, len(open), ciVerdict)
	case len(open) > 0:
		s.logf(name, "review round %d: %d thread(s) open", round, len(open))
	default:
		s.logf(name, "review round %d: threads clear, waiting on CI (%s)", round, ciVerdict)
	}
	return false
}
