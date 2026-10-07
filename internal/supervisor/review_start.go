package supervisor

import (
	"context"
	"os"
	"sync"
	"time"

	"pi-supervisor/internal/job"
	"pi-supervisor/internal/journal"
	"pi-supervisor/internal/review"
)

// reviewClients caches one GitHub client per owner/repo.
//
// DRY: the client owns the token source (App installation token with its own
// 1h refresh), so building one per verb call would re-resolve the App
// installation on every poll and multiply credential traffic. It is built
// lazily on first use so a daemon that never reviews never touches GitHub
// credentials at all.
type reviewClients struct {
	mu sync.Mutex
	m  map[string]*review.Client
}

func newReviewClients() *reviewClients {
	return &reviewClients{m: map[string]*review.Client{}}
}

func (c *reviewClients) get(ctx context.Context, owner, repo string) (*review.Client, error) {
	key := owner + "/" + repo
	c.mu.Lock()
	if cl, ok := c.m[key]; ok {
		c.mu.Unlock()
		return cl, nil
	}
	c.mu.Unlock()
	// Built outside the lock: the App installation lookup is a network call
	// and two concurrent verbs must not both pay for it.
	cl, err := review.NewClient(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Another goroutine may have won the race; keep whichever landed first so
	// there is exactly one token source per repo.
	if existing, ok := c.m[key]; ok {
		return existing, nil
	}
	c.m[key] = cl
	return cl, nil
}

// listOpenThreads is the seam the post-completion re-check reads threads
// through, so the ALERT path is testable without network access.
//
// It exists for the same reason newSessionResolver does: the re-check's whole
// job is a comparison and an emit, and neither was reachable in a test because
// the only way in was a live GitHub client. Without this seam every assertion
// about "threads appeared" stops at "the code tried to dial GitHub", which is
// how a decision bug can ship with a green suite.
//
// Production never reassigns it.
// It is bound to a Supervisor in New() rather than reading the package-level
// default, so it can reuse s.reviewClient — the per-repo client CACHE. Building a
// client per call instead re-resolved the GitHub App installation token every
// time, which is exactly the multiply-the-credential-traffic cost the cache
// exists to avoid, and this runs once per round per job.
var listOpenThreads = func(s *Supervisor, ctx context.Context, owner, repo string, pr int) (int, error) {
	cl, err := s.reviewClient(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	all, err := cl.ListThreads(ctx, owner, repo, pr)
	if err != nil {
		return 0, err
	}
	return len(review.OpenThreads(all)), nil
}

// reviewClient is the Supervisor's accessor for the cached client.
func (s *Supervisor) reviewClient(ctx context.Context, owner, repo string) (*review.Client, error) {
	return s.ghClients.get(ctx, owner, repo)
}

// freshCampaignSession gives a newly armed campaign its OWN session (ADR-0018).
//
// With review_brief set, the build session pin is cleared and persisted, so
// the campaign's round 1 LAUNCHes a fresh session seeded with the review
// brief; every later campaign round resumes THAT session (invariant 1 holds
// within the campaign). Without it the campaign would resume the finished
// build session, whose own context says the work is complete — observed live
// (mealime-search3, flambette#58): every round re-verified old work and
// exited in ~40s, zero threads addressed, budget exhausted.
//
// With review_brief empty the pin stays (legacy behavior) and the daemon
// warns: resuming a finished build session is the known-poison path.
//
// Called at ARM time only — never on a plain Start: a stop mid-campaign plus
// a watch-driven resume (ADR-0017) must resume the campaign's own session,
// not fork it.
func (s *Supervisor) freshCampaignSession(r *runner, name string) {
	r.mu.Lock()
	brief := r.job.ReviewBrief
	pinned := r.job.SessionPath
	if brief != "" {
		r.job.SessionPath = ""
		updated := r.job
		r.mu.Unlock()
		// Persist BEFORE the first round reads the job: the cleared pin must
		// survive a daemon restart between arm and round 1.
		_ = job.Save(updated)
		buildBytes := int64(0)
		if fi, err := os.Stat(pinned); err == nil {
			buildBytes = fi.Size()
		}
		s.logf(name, "campaign armed with its own session: cleared build pin (%s, %d bytes), round 1 will LAUNCH review brief %s",
			pinned, buildBytes, brief)
		return
	}
	r.mu.Unlock()
	if pinned != "" {
		journal.Subsys("job").Warn(
			"campaign armed WITHOUT review_brief — resuming the build session; its own context says the work is complete, so rounds may re-verify and exit without addressing threads (ADR-0018). Add \"review_brief\" to the job def.",
			"job", name)
	}
}

// StartReview arms a review campaign on a job for a PR (ADR-0012 §4) and
// LAUNCHES it in the same call (ADR-0016 owner correction): the campaign
// takes over the job's round loop immediately, resuming the same session
// (never re-launching — a second LAUNCH forks the session and splits the
// work). An armed-but-not-started state no longer exists.
func (s *Supervisor) StartReview(ctx context.Context, name string, pr, rounds int, kind string) (any, error) {
	s.mu.Lock()
	r, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		return nil, review.ErrUnknownJob(name)
	}
	r.mu.Lock()
	if r.active {
		r.mu.Unlock()
		return nil, review.ErrUsage("job %q is already running; stop it before arming a review", name)
	}
	owner, repo, target, ok := reviewTarget(r.job.Worktree, r.state.PRURL, pr)
	if !ok {
		r.mu.Unlock()
		return nil, review.ErrUsage("cannot determine the GitHub repo/PR for job %q (pass --pr with a linked pr_url, or set pr_url via the transcript)", name)
	}
	if rounds <= 0 {
		rounds = s.reviewDefaultRounds()
	}
	camp := newReviewCampaign(owner, repo, target, rounds, kind)
	r.review = camp
	r.mu.Unlock()
	s.freshCampaignSession(r, name)
	_ = ctx
	s.emit(name, "review_armed", 0, 0, 0, "",
		"review armed on %s/%s#%d for %d round(s), type=%s", owner, repo, target, rounds, kind)
	// Confirm the PR is open before spending rounds on a merged PR.
	gh, err := s.reviewClient(ctx, owner, repo)
	if err == nil {
		if open, err := gh.PROpen(ctx, owner, repo, target); err == nil && !open {
			return nil, review.ErrUsage("PR %s/%s#%d is not open", owner, repo, target)
		}
	}
	// ADR-0016 (owner correction): arming LAUNCHES the campaign directly.
	// The old two-step (arm, then a separate `start`) read as a dead button —
	// the arm answered, nothing ran, and the operator had to know the secret
	// second command. The auto-review path already went gate→reviewing in one
	// step; the manual path now does too. Start() enters `reviewing` because
	// r.review is set (invariant 23), resuming the pinned session.
	if err := s.Start(name); err != nil {
		return nil, review.ErrUsage("campaign armed but the round loop refused to start: %v", err)
	}
	return map[string]any{
		"job": name, "owner": owner, "repo": repo, "pr": target,
		"rounds": rounds, "type": kind,
	}, nil
}

// ArmAutoReview writes the auto_review stanza flag on a job so its completion
// gate arms a review when the marker lands (ADR-0012 §4.1). It is one-shot:
// the flag is consumed when the trigger fires, because the marker latch is
// sticky and an unguarded re-arm would loop forever.
func (s *Supervisor) ArmAutoReview(name string, rounds int, kind string) (any, error) {
	s.mu.Lock()
	r, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		return nil, review.ErrUnknownJob(name)
	}
	r.mu.Lock()
	r.autoReview = &autoReviewSpec{rounds: rounds, kind: kind}
	r.review = nil
	r.mu.Unlock()
	s.emit(name, "review_auto_armed", 0, 0, 0, "",
		"auto review armed; fires once when the completion gate closes with an open PR")
	return map[string]any{"job": name, "auto": true, "rounds": rounds, "type": kind}, nil
}

// autoReviewSpec is the persisted intent to auto-arm a review.
type autoReviewSpec struct {
	rounds int
	kind   string
}

// reviewDefaultRounds reads the configured default round budget.
func (s *Supervisor) reviewDefaultRounds() int {
	s.mu.Lock()
	n := s.reviewRounds
	s.mu.Unlock()
	if n <= 0 {
		return 5
	}
	return n
}

// ExpireAcks runs ack expiry on a ticker for the life of the daemon, so an
// unacked bulk_resolve cannot wedge a campaign nobody is watching.
func (s *Supervisor) ExpireAcks(stop chan struct{}) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.ExpireReviewAcks()
		}
	}
}
