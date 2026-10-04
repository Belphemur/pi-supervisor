package supervisor

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"pi-supervisor/internal/job"
	"pi-supervisor/internal/review"
)

// Post-completion review-thread detection (ADR-0012 follow-up).
//
// The gap this closes, observed live on PR #3: a review campaign closed clean
// at 17:26, and bot findings landed at 17:33 and 17:53. Every review verb
// requires a live round (no-live-round, exit 2) and the auto-trigger stanza is
// consumed one-shot, so those two findings had no answering round. They were
// only found because a human went looking.
//
// The fix records a BASELINE when the campaign ends — open-thread count plus the
// git HEAD it reviewed — and compares a fresh count against it later.
//
// Why this is not a poller (the whole design constraint):
//
// A ticker that re-lists threads forever burns API quota, keeps the token warm
// for no reason, and violates the push-only delivery rule (AGENTS.md invariant
// 6), where cronjob and polling paths are explicit non-goals. So the check is
// EDGE-TRIGGERED off events the daemon already observes or the operator asks
// for:
//
//   - a push (git HEAD moved), and
//   - an explicit `pi-supervisor review recheck <job>`.
//
// Both are real events; neither is a timer. Cost is one GraphQL list per push
// to a job that has a baseline — bounded by pushes, not by wall-clock.

// recordThreadBaseline stores the open-thread count and reviewed HEAD when a
// campaign ends, so a later re-check has something to compare against.
//
// openAtClose is passed rather than re-derived: a campaign that ends on
// review_exhausted has threads STILL open, and hardcoding 0 there would record
// a baseline that makes the next re-check look like a flood of new findings.
func (s *Supervisor) recordThreadBaseline(name string, r *runner, owner, repo string, pr, openAtClose int) {
	head := s.headSHA(r)
	r.mu.Lock()
	r.state.ReviewBaseline = &job.ReviewBaseline{
		Owner: owner, Repo: repo, PR: pr,
		OpenAtClose: openAtClose,
		Head:        head,
		ClosedAt:    time.Now().UTC().Format(time.RFC3339),
		OpenNow:     openAtClose,
	}
	r.mu.Unlock()
	r.persistState()
	s.logf(name, "review baseline recorded: %d open thread(s) at %s on %s/%s#%d",
		openAtClose, shortSHA(head), owner, repo, pr)
}

// recheckThreads compares the current open-thread count against the recorded
// baseline and emits review_threads_appeared when the PR gained findings after
// the campaign ended.
//
// No-op when there is no baseline (the job never reviewed anything), which keeps
// the cost at zero for build-only jobs.
func (s *Supervisor) recheckThreads(name string) {
	r := s.jobs[name]
	if r == nil {
		return
	}
	r.mu.Lock()
	// A live campaign owns the threads right now: a re-check mid-campaign
	// would report the campaign's own in-flight replies as brand-new findings.
	if r.active {
		r.mu.Unlock()
		return
	}
	var bl job.ReviewBaseline
	if r.state.ReviewBaseline == nil {
		r.mu.Unlock()
		return
	}
	bl = *r.state.ReviewBaseline
	r.mu.Unlock()

	if bl.Owner == "" || bl.Repo == "" || bl.PR == 0 {
		return
	}

	head := s.headSHA(r)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cli, err := s.reviewClient(ctx, bl.Owner, bl.Repo)
	if err != nil {
		s.logf(name, "thread re-check skipped: %v", err)
		return
	}
	all, err := cli.ListThreads(ctx, bl.Owner, bl.Repo, bl.PR)
	if err != nil {
		s.logf(name, "thread re-check failed: %v", err)
		return
	}
	openNow := len(review.OpenThreads(all))
	delta := openNow - bl.OpenAtClose

	r.mu.Lock()
	if r.state.ReviewBaseline != nil {
		r.state.ReviewBaseline.OpenNow = openNow
		r.state.ReviewBaseline.CheckedAt = time.Now().UTC().Format(time.RFC3339)
		r.state.ReviewBaseline.NewSinceClose = max(delta, 0)
	}
	r.mu.Unlock()
	r.persistState()

	if delta <= 0 {
		s.logf(name, "thread re-check: %d open (baseline %d) — nothing new", openNow, bl.OpenAtClose)
		return
	}

	moved := ""
	if head != bl.Head {
		moved = fmt.Sprintf(" (HEAD moved %s -> %s since the campaign closed)",
			shortSHA(bl.Head), shortSHA(head))
	}
	s.logf(name, "review_threads_appeared: %d new thread(s) after the campaign closed%s", delta, moved)
	s.emit(name, "review_threads_appeared", 0, 0, 0, "",
		"%d new review thread(s) appeared on %s/%s#%d AFTER the review campaign closed (%d -> %d open)%s — "+
			"the campaign is one-shot by design and every review verb needs a live round, so these have no answering round. "+
			"Re-arm: pi-supervisor review %s --pr %d",
		delta, bl.Owner, bl.Repo, bl.PR, bl.OpenAtClose, openNow, moved, name, bl.PR)
}

// headSHA is the worktree's current HEAD, or "" when unavailable. Used only as
// a change signal, so an error degrades to "" instead of failing a re-check.
func (s *Supervisor) headSHA(r *runner) string {
	r.mu.Lock()
	wt := r.job.Worktree
	r.mu.Unlock()
	if wt == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func shortSHA(sha string) string {
	switch {
	case sha == "":
		return "(unknown)"
	case len(sha) > 7:
		return sha[:7]
	default:
		return sha
	}
}
