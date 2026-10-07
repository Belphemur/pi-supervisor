package supervisor

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
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
	// s.jobs is mutated by LoadJobs under s.mu; reading it unlocked is a data
	// race that can crash the daemon, not just miss a job.
	s.mu.Lock()
	r := s.jobs[name]
	s.mu.Unlock()
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
	openNow, err := s.listOpen(ctx, bl.Owner, bl.Repo, bl.PR)
	if err != nil {
		s.logf(name, "thread re-check failed: %v", err)
		return
	}
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

// campaignSnapshot returns the live campaign's identity, or nil when none is
// armed. It exists so the exhausted path can record a baseline BEFORE the
// campaign is torn down, while owner/repo/pr are still reachable.
func (r *runner) campaignSnapshot() *reviewCampaign {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.review
}

// jobResult is one job's verdict in a repo-wide re-check. Package scope so the
// regression tests can assert on the slice the daemon actually returns.
type jobResult struct {
	Job          string `json:"job"`
	Checked      bool   `json:"checked"`
	OpenAtClose  int    `json:"open_at_close"`
	OpenNow      int    `json:"open_now"`
	NewThreads   int    `json:"new_threads"`
	Action       string `json:"action"`
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	PR           int    `json:"pr"`
	CampaignBusy bool   `json:"campaign_active"`
	Error        string `json:"error,omitempty"`
}

// recheckAllResult is the repo-wide re-check's reply shape. Named so callers
// (and tests) can assert on it without going through JSON.
type recheckAllResult struct {
	Jobs     []jobResult `json:"jobs"`
	Alerted  int         `json:"alerted"`
	Skipped  int         `json:"skipped"`
	ScopeStr string      `json:"scope"`
}

// RecheckThreadsAll is the post-push hook's entry point: re-check every job
// that has a recorded review baseline, optionally narrowed to the jobs whose
// baseline PR lives in `repoSlug` (owner/name).
//
// Narrowing matters. A push to one repo must not spend API calls re-checking
// jobs whose PRs live elsewhere — and on a busy machine that fan-out is the
// difference between one GraphQL list and dozens. With `pushed` set and no
// slug, the caller means "this repository", resolved from the worktree's origin.
//
// Per-job failures are collected, not fatal: one job with a dead token must not
// hide the verdict for the others.
func (s *Supervisor) RecheckThreadsAll(ctx context.Context, pushed bool, repoSlug string) (any, error) {
	s.mu.Lock()
	names := make([]string, 0, len(s.jobs))
	for n := range s.jobs {
		names = append(names, n)
	}
	s.mu.Unlock()
	sort.Strings(names)

	out := recheckAllResult{ScopeStr: "all jobs with a review baseline"}

	wantRepo, wantOwner := "", ""
	if repoSlug != "" {
		wantOwner, wantRepo, _ = strings.Cut(repoSlug, "/")
		out.ScopeStr = "jobs whose review PR is in " + repoSlug
	} else if pushed {
		wantOwner, wantRepo = s.originRepo()
		out.ScopeStr = "jobs whose review PR is in this repository"
		if wantRepo == "" {
			out.ScopeStr += " (origin unresolvable; checked every job instead)"
		}
	}

	for _, n := range names {
		s.mu.Lock()
		r := s.jobs[n]
		s.mu.Unlock()
		if r == nil {
			continue
		}
		r.mu.Lock()
		hasBaseline := r.state.ReviewBaseline != nil
		var blOwner, blRepo string
		var blPR int
		if hasBaseline {
			blOwner, blRepo, blPR = r.state.ReviewBaseline.Owner, r.state.ReviewBaseline.Repo, r.state.ReviewBaseline.PR
		}
		r.mu.Unlock()
		if !hasBaseline {
			out.Skipped++
			continue
		}
		// Case-insensitive: GitHub owner/repo casing is not stable across
		// transcript scrapes, REST payloads and git remotes.
		if wantRepo != "" &&
			(!strings.EqualFold(blOwner, wantOwner) || !strings.EqualFold(blRepo, wantRepo)) {
			out.Skipped++
			continue
		}

		res, err := s.RecheckThreads(ctx, n)
		if err != nil {
			out.Jobs = append(out.Jobs, jobResult{Job: n, Action: "re-check failed: " + err.Error(), Error: err.Error()})
			continue
		}
		m, _ := res.(map[string]any)
		jr := jobResult{
			Job: n, Owner: blOwner, Repo: blRepo, PR: blPR,
			OpenAtClose: intOf(m["open_at_close"]), OpenNow: intOf(m["open_now"]),
			NewThreads: intOf(m["new_threads"]), Checked: true,
			Action: strOf(m["action"]), CampaignBusy: boolOf(m["campaign_active"]),
		}
		if jr.NewThreads > 0 {
			out.Alerted++
		}
		out.Jobs = append(out.Jobs, jr)
	}
	return out, nil
}

// originRepo reads owner/name from a job worktree's origin remote. Used to
// scope a push-triggered re-check to the repo that was actually pushed.
//
// s.jobs is read under s.mu and the map is COPIED before iterating: ranging a
// live map without the lock is a data race, and LoadJobs deletes from it while a
// re-check may be running.
func (s *Supervisor) originRepo() (owner, repo string) {
	s.mu.Lock()
	worktrees := make([]string, 0, len(s.jobs))
	for _, j := range s.jobs {
		j.mu.Lock()
		if j.job.Worktree != "" {
			worktrees = append(worktrees, j.job.Worktree)
		}
		j.mu.Unlock()
	}
	s.mu.Unlock()
	for _, wt := range worktrees {
		if owner, repo = parseGitHubRemote(wt); owner != "" {
			return owner, repo
		}
	}
	return "", ""
}

// parseGitHubRemote resolves a worktree's origin remote to owner/name, or
// ("","") when it is absent, unreadable, or not a GitHub remote we can split.
func parseGitHubRemote(wt string) (owner, repo string) {
	if wt == "" {
		return "", ""
	}
	// git -C does NOT override an inherited GIT_DIR: the hook environment
	// exports GIT_DIR (pre-push runs this via review-recheck), and without
	// scrubbing, every probe would read the SUPERVISOR's repo instead of the
	// job's worktree — scoping every job to the wrong remote. Same GIT_*
	// class as the test bug fixed in review_origin_test.go.
	cmd := exec.Command("git", "-C", wt, "remote", "get-url", "origin")
	cmd.Env = envWithoutGitVars()
	out, err := cmd.Output()
	if err != nil {
		return "", ""
	}
	u := strings.TrimSpace(string(out))
	// Accept both the SSH and HTTPS spellings; anything else is not a repo we
	// can scope by, and the caller falls back to checking everything.
	for _, prefix := range []string{"git@github.com:", "https://github.com/", "http://github.com/", "ssh://git@github.com/"} {
		after, ok := strings.CutPrefix(u, prefix)
		if !ok {
			continue
		}
		rest := strings.TrimSuffix(after, ".git")
		owner, repo, ok := strings.Cut(rest, "/")
		if !ok || owner == "" || repo == "" {
			// A remote we cannot split into owner/repo cannot scope anything;
			// the caller falls back to checking every job.
			return "", ""
		}
		return owner, repo
	}
	return "", ""
}

func intOf(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	default:
		return 0
	}
}

func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

// RecheckThreads is the operator-facing `review --recheck`: it compares the
// PR's current open-thread count against the recorded baseline and RETURNS the
// verdict, so a caller gets an answer synchronously instead of having to infer
// one from a later event.
//
// It is deliberately NOT gated on a live round. Every other review verb is
// (ADR-0012 §4), because those verbs post to GitHub on the job's behalf. This
// one only READS thread state, so allowing it outside a round is safe and is the
// whole point: the findings it reports are precisely the ones that arrived when
// no round existed to answer them.
func (s *Supervisor) RecheckThreads(ctx context.Context, name string) (any, error) {
	s.mu.Lock()
	r, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		return nil, review.ErrUnknownJob(name)
	}
	r.mu.Lock()
	if r.state.ReviewBaseline == nil {
		r.mu.Unlock()
		// Not an error: the job never ran a campaign, so there is no baseline
		// and nothing to compare. Say so plainly instead of refusing.
		return map[string]any{
			"job": name, "checked": false,
			"message": "no review baseline recorded — this job never ran a review campaign, so there is nothing to re-check",
		}, nil
	}
	bl := *r.state.ReviewBaseline
	active := r.active
	r.mu.Unlock()

	if bl.Owner == "" || bl.Repo == "" || bl.PR == 0 {
		return nil, review.ErrUsage("the recorded review baseline for %s names no PR; re-arm the campaign", name)
	}

	openNow, err := s.listOpen(ctx, bl.Owner, bl.Repo, bl.PR)
	if err != nil {
		return nil, err
	}
	delta := openNow - bl.OpenAtClose

	r.mu.Lock()
	if r.state.ReviewBaseline != nil {
		r.state.ReviewBaseline.OpenNow = openNow
		r.state.ReviewBaseline.CheckedAt = time.Now().UTC().Format(time.RFC3339)
		r.state.ReviewBaseline.NewSinceClose = max(delta, 0)
	}
	r.mu.Unlock()
	r.persistState()

	out := map[string]any{
		"job": name, "checked": true,
		"owner": bl.Owner, "repo": bl.Repo, "pr": bl.PR,
		"open_at_close":   bl.OpenAtClose,
		"open_now":        openNow,
		"new_threads":     max(delta, 0),
		"campaign_active": active,
	}
	if head := s.headSHA(r); head != bl.Head {
		out["head_at_close"] = bl.Head
		out["head_now"] = head
	}
	if delta > 0 {
		// The campaign already ended (or is inactive); a live campaign owns its
		// own threads, so only warn when the job is not mid-campaign.
		if !active {
			s.logf(name, "review_threads_appeared: %d new thread(s) after the campaign closed", delta)
			s.emit(name, "review_threads_appeared", 0, 0, 0, "",
				"%d new review thread(s) appeared on %s/%s#%d AFTER the review campaign closed (%d -> %d open) — "+
					"the campaign is one-shot by design and every review verb needs a live round, so these have no answering round. "+
					"Re-arm: pi-supervisor review %s --pr %d",
				delta, bl.Owner, bl.Repo, bl.PR, bl.OpenAtClose, openNow, name, bl.PR)
		}
		out["action"] = "re-arm with: pi-supervisor review " + name + " --pr " + itoa(bl.PR)
	} else {
		out["action"] = "nothing new since the campaign closed"
	}
	return out, nil
}

func itoa(n int) string { return strconv.Itoa(n) }

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
