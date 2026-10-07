package supervisor

import (
	"context"
	"time"

	"pi-supervisor/internal/review"
)

// codeRabbitTrigger is the fixed trigger comment (ADR-0012 §8, Q1). Hardcoded
// on purpose: one bot, one convention. A config knob for a string that never
// varies in this deployment is surface without a user.
const codeRabbitTrigger = "@coderabbitai review"

// autoReviewHandoff decides whether the completion gate hands the job to a
// review campaign, and if so arms it (ADR-0012 §4.1).
//
// Returns true when the caller should transition the job done -> reviewing.
// The whole decision is made under the runner's lock so the build job's loop
// and the campaign never both think they own the round.
//
// The stanza is ONE-SHOT: it is consumed here whether or not the PR turns out
// to be usable. The marker latch is sticky (ADR-0011), so leaving it armed
// would re-arm on every subsequent round and loop forever.
func (s *Supervisor) autoReviewHandoff(name string, r *runner) bool {
	r.mu.Lock()
	spec := r.autoReview
	if spec == nil {
		r.mu.Unlock()
		return false
	}
	// Consume unconditionally: one trigger per armed stanza.
	r.autoReview = nil
	prURL := r.state.PRURL
	worktree := r.job.Worktree
	r.mu.Unlock()

	if prURL == "" {
		// No linked PR: nothing to review. Not an error — "" means "not
		// linked", never "no PR exists" (ADR-0006).
		s.emit(name, "review_skipped", 0, 0, 0, "",
			"auto review skipped: no GitHub PR was linked in the transcript")
		return false
	}
	owner, repo, pr, ok := reviewTarget(worktree, prURL, 0)
	if !ok {
		s.emit(name, "review_skipped", 0, 0, 0, "",
			"auto review skipped: cannot parse a PR number out of %q", prURL)
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	gh, err := s.reviewClient(ctx, owner, repo)
	if err != nil {
		s.emit(name, "review_skipped", 0, 0, 0, "", "auto review skipped: %v", err)
		return false
	}
	open, err := gh.PROpen(ctx, owner, repo, pr)
	if err != nil {
		s.emit(name, "review_skipped", 0, 0, 0, "",
			"auto review skipped: cannot read PR state: %v", err)
		return false
	}
	if !open {
		s.emit(name, "review_skipped", 0, 0, 0, "",
			"auto review skipped: PR %s/%s#%d is not open", owner, repo, pr)
		return false
	}

	// Trigger CodeRabbit. A fresh PR legitimately has ZERO threads until the
	// bot finishes its pass — which is what the warmup below exists for.
	if err := gh.PostComment(ctx, owner, repo, pr, codeRabbitTrigger); err != nil {
		s.emit(name, "review_skipped", 0, 0, 0, "",
			"auto review skipped: could not post the CodeRabbit trigger: %v", err)
		return false
	}

	rounds := spec.rounds
	if rounds <= 0 {
		rounds = s.reviewDefaultRounds()
	}
	camp := newReviewCampaign(owner, repo, pr, rounds, spec.kind)

	// Warmup, then re-check. The count is read AFTER the wait, never before:
	// checking early is exactly the "0 threads means clean review" mistake the
	// warmup exists to prevent.
	warmup := s.reviewWarmup()
	select {
	case <-s.stop:
		return false
	case <-time.After(warmup):
	}
	threads, err := gh.ListThreads(ctx, owner, repo, pr)
	if err != nil {
		s.emit(name, "review_skipped", 0, 0, 0, "",
			"auto review skipped: could not list threads after warmup: %v", err)
		return false
	}
	openThreads := review.OpenThreads(threads)
	if len(openThreads) == 0 {
		s.emit(name, "review_skipped", 0, 0, 0, "",
			"no open threads after CodeRabbit warmup (%s) on %s/%s#%d", warmup, owner, repo, pr)
		return false
	}

	r.mu.Lock()
	r.review = camp
	r.mu.Unlock()
	s.freshCampaignSession(r, name)
	s.logf(name, "auto review armed on %s/%s#%d with %d open thread(s), %d round(s)",
		owner, repo, pr, len(openThreads), rounds)
	s.emit(name, "review_armed", 0, 0, 0, "",
		"auto review armed on %s/%s#%d: %d open thread(s), %d round(s), type=%s",
		owner, repo, pr, len(openThreads), rounds, spec.kind)
	return true
}

// reviewWarmup is how long to wait for CodeRabbit to produce threads.
func (s *Supervisor) reviewWarmup() time.Duration {
	s.mu.Lock()
	d := s.reviewWarmupDur
	s.mu.Unlock()
	if d <= 0 {
		return 5 * time.Minute
	}
	return d
}
