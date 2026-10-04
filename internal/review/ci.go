package review

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-github/v90/github"
)

// CICheck is one check run observed on the PR's head sha.
type CICheck struct {
	Name       string `json:"name"`
	Status     string `json:"status"` // queued|in_progress|completed
	Conclusion string `json:"conclusion"`
	// Required marks a check the base branch's protection rules demand. ONLY a
	// required check blocks (ADR-0012 §3.3).
	Required bool `json:"required"`
}

// CIRollup is the CI verdict for one head sha.
type CIRollup struct {
	// Verdict is pass|pending|fail and considers REQUIRED checks only.
	Verdict string `json:"verdict"`
	HeadSHA string `json:"head_sha,omitempty"`
	// Blocking is the subset that gates the campaign: a failing or still-running
	// required check.
	Blocking []CICheck `json:"blocking,omitempty"`
	// NonBlocking lists failing OPTIONAL checks: reported so the operator can
	// see them, deliberately excluded from Verdict.
	NonBlocking []CICheck `json:"non_blocking,omitempty"`
	// All is every check observed, required or not.
	All []CICheck `json:"checks,omitempty"`
	// RequiredUnknown is set when protection exists but its required list could
	// not be read. The verdict then treats EVERY check as required (fail
	// closed): it can delay a merge, never wave a red required check through.
	RequiredUnknown bool `json:"required_unknown,omitempty"`
	// NoProtection is set when the base branch requires no status checks at all.
	// Nothing is required, so nothing blocks — the opposite of RequiredUnknown,
	// and conflating the two would deadlock every campaign on an unprotected
	// repo, since no check could ever pass an empty requirement list.
	NoProtection bool `json:"no_protection,omitempty"`
	// BaseBranch is the branch the PR targets, for the operator's benefit.
	BaseBranch string `json:"base_branch,omitempty"`
}

// CheckCI returns the CI verdict for the PR's CURRENT head sha.
//
// Three properties, each replacing something that was wrong when this was first
// written (ADR-0012 §3.3):
//
//   - It reads the commit's check runs, not a `gh pr checks` rollup, because that
//     rollup races state transitions right after a push and transiently reads
//     green with jobs still queued.
//   - Required-ness comes from the BASE BRANCH's branch-protection rules, which
//     is the only place that information exists. Neither the check-runs REST
//     endpoint nor the Actions /jobs endpoint carries a required flag, and a
//     live test showed GraphQL `checkRun.isRequired(pullRequestId:)` returns
//     nothing for a fork PR whose base is unprotected.
//   - It scopes to the head sha observed at poll time, so a push landing
//     mid-poll cannot produce a verdict for the wrong commit.
func (c *Client) CheckCI(ctx context.Context, owner, repo string, pr int) (*CIRollup, error) {
	head, err := c.PRHeadSHA(ctx, owner, repo, pr)
	if err != nil {
		return nil, err
	}
	pull, _, err := c.rest.PullRequests.Get(ctx, owner, repo, pr)
	if err != nil {
		return nil, classifyGH(restErr(err))
	}
	base := pull.GetBase().GetRef()

	checks, err := c.commitChecks(ctx, owner, repo, head)
	if err != nil {
		return nil, err
	}

	required, known, unprotected := c.requiredContexts(ctx, owner, repo, base)
	roll := rollup(head, checks, required, known)
	roll.BaseBranch = base
	roll.NoProtection = unprotected
	return roll, nil
}

// commitChecks lists every check run on the head sha, paging to exhaustion.
//
// /commits/{sha}/check-runs is the same data GitHub's own UI renders and needs
// no per-workflow fan-out; the Actions /jobs walk is kept only as a fallback for
// repos where that endpoint is unavailable.
func (c *Client) commitChecks(ctx context.Context, owner, repo, sha string) ([]CICheck, error) {
	var out []CICheck
	opt := &github.ListCheckRunsOptions{
		// latest: only the newest run per check name, which is what a merge
		// gate cares about. "all" would report stale re-runs too.
		Filter:  new("latest"),
		PerPage: 100,
	}
	for {
		res, resp, err := c.rest.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, opt)
		if err != nil {
			jobs, ferr := c.workflowJobChecks(ctx, owner, repo, sha)
			if ferr != nil {
				return nil, classifyGH(restErr(err))
			}
			return jobs, nil
		}
		for _, run := range res.CheckRuns {
			out = append(out, CICheck{
				Name:       run.GetName(),
				Status:     strings.ToLower(run.GetStatus()),
				Conclusion: strings.ToLower(run.GetConclusion()),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// workflowJobChecks is the fallback source: the Actions /jobs endpoint for every
// workflow run on the head sha.
func (c *Client) workflowJobChecks(ctx context.Context, owner, repo, sha string) ([]CICheck, error) {
	runs, _, err := c.rest.Actions.ListRepositoryWorkflowRuns(ctx, owner, repo, &github.ListWorkflowRunsOptions{
		HeadSHA: sha,
		PerPage: 10,
	})
	if err != nil || runs == nil || len(runs.WorkflowRuns) == 0 {
		return nil, err
	}
	var out []CICheck
	for _, run := range runs.WorkflowRuns {
		jobs, _, jerr := c.rest.Actions.ListWorkflowJobs(ctx, owner, repo, run.GetID(), &github.ListWorkflowJobsOptions{
			PerPage: 100,
		})
		if jerr != nil {
			return nil, jerr
		}
		for _, j := range jobs.Jobs {
			out = append(out, CICheck{
				Name:       j.GetName(),
				Status:     strings.ToLower(j.GetStatus()),
				Conclusion: strings.ToLower(j.GetConclusion()),
			})
		}
	}
	return out, nil
}

// requiredContexts returns the check contexts the base branch's protection
// requires.
//
// Three outcomes, and the difference between the last two is load-bearing:
//
//   - known && !unprotected: the list is authoritative.
//   - unprotected: the branch requires no status checks, so NOTHING is required
//     and nothing blocks.
//   - !known: protection exists but the list was unreadable (typically a token
//     without admin scope). The caller fails CLOSED.
//
// A 404 is the documented "branch not protected" answer, not an error: conflating
// it with "unreadable" is what would deadlock campaigns on unprotected repos.
//
// go-github does NOT hand back an *ErrorResponse for this case — it converts
// the 404 into a plain errors.New("branch is not protected") and drops the
// response. So the message is the only signal available, which is why this
// checks the string as well as the status (a live test caught the mismatch:
// with only the ErrorResponse check, an unprotected branch reported
// required_unknown and every check was treated as required).
func (c *Client) requiredContexts(ctx context.Context, owner, repo, base string) (required map[string]bool, known, unprotected bool) {
	prot, resp, err := c.rest.Repositories.GetBranchProtection(ctx, owner, repo, base)
	if err != nil {
		if isNotProtected(err, resp) {
			return map[string]bool{}, true, true // nothing required, nothing blocks
		}
		// Forbidden (no admin scope) or any other failure: we cannot tell, so
		// the caller fails closed.
		return nil, false, false
	}
	if prot == nil || prot.RequiredStatusChecks == nil {
		return map[string]bool{}, true, true // protected, but requires no checks
	}
	out := map[string]bool{}
	// v90 models both lists as POINTERS to slices, so each needs a nil guard.
	if ctxs := prot.RequiredStatusChecks.Contexts; ctxs != nil {
		for _, name := range *ctxs {
			out[name] = true
		}
	}
	if checks := prot.RequiredStatusChecks.Checks; checks != nil {
		for _, chk := range *checks {
			if chk != nil && chk.Context != "" {
				out[chk.Context] = true
			}
		}
	}
	if len(out) == 0 {
		return out, true, true
	}
	return out, true, false
}

// isNotProtected recognizes "this branch has no protection rules" from either
// signal go-github offers: the error text it synthesizes, or a 404 response.
func isNotProtected(err error, resp *github.Response) bool {
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "not protected") {
		return true
	}
	var rerr *github.ErrorResponse
	if errors.As(err, &rerr) && rerr.Response != nil &&
		rerr.Response.StatusCode == http.StatusNotFound {
		return true
	}
	return resp != nil && resp.Response != nil &&
		resp.Response.StatusCode == http.StatusNotFound
}

// failing reports whether a completed check failed. An empty conclusion means
// the run has not completed and is handled by the pending case instead.
func failing(conclusion string) bool {
	switch conclusion {
	// GitHub uses the British spelling for this conclusion; matching the wire
	// vocabulary is the point.
	case "failure", "cancelled", "error", "timed_out", "action_required", "startup_failure": //nolint:misspell // GitHub API spelling
		return true
	}
	return false
}

// stillRunning reports whether a check has not reached a conclusion.
func stillRunning(status string) bool {
	switch status {
	case "", "queued", "in_progress", "waiting", "requested", "pending":
		return true
	}
	return false
}

// rollup folds observed checks into a verdict over REQUIRED checks only.
//
// neutral/skipped count as passing (inherited from pre-merge's gate): they are
// deliberate opt-outs, not failures.
func rollup(head string, checks []CICheck, required map[string]bool, known bool) *CIRollup {
	out := &CIRollup{HeadSHA: head, Verdict: "pass", RequiredUnknown: !known}
	out.All = make([]CICheck, 0, len(checks))
	fail, pending := false, false
	for _, chk := range checks {
		// Unknown required-ness => treat everything as required (fail closed):
		// a spurious "not done" costs rounds, a missed red check costs a merge.
		chk.Required = !known || requiredMatches(required, chk.Name)
		out.All = append(out.All, chk)
		switch {
		case stillRunning(chk.Status):
			if chk.Required {
				pending = true
				out.Blocking = append(out.Blocking, chk)
			}
		case failing(chk.Conclusion):
			if chk.Required {
				fail = true
				out.Blocking = append(out.Blocking, chk)
			} else {
				out.NonBlocking = append(out.NonBlocking, chk)
			}
		}
	}
	switch {
	case fail:
		out.Verdict = "fail"
	case pending:
		out.Verdict = "pending"
	}
	return out
}

// requiredMatches reports whether a check run's NAME corresponds to a required
// context. Protection stores contexts (older style, e.g. "ci/circleci") while
// check runs report names, so an exact hit is tried first and a case-insensitive
// match second.
func requiredMatches(required map[string]bool, name string) bool {
	if len(required) == 0 {
		return false
	}
	if required[name] {
		return true
	}
	lower := strings.ToLower(name)
	for ctxName := range required {
		if strings.ToLower(ctxName) == lower {
			return true
		}
	}
	return false
}

// BlockingSummary renders the blocking checks for a human/LLM message.
func (r *CIRollup) BlockingSummary() string {
	if len(r.Blocking) == 0 {
		return "none"
	}
	return fmtChecks(r.Blocking)
}

// NonBlockingSummary renders the failing optional checks, or "" when none.
func (r *CIRollup) NonBlockingSummary() string {
	if len(r.NonBlocking) == 0 {
		return ""
	}
	return fmtChecks(r.NonBlocking)
}

func fmtChecks(cs []CICheck) string {
	var out strings.Builder
	for i, c := range cs {
		if i > 0 {
			out.WriteString(", ")
		}
		state := c.Conclusion
		if state == "" {
			state = c.Status
		}
		fmt.Fprintf(&out, "%s=%s", c.Name, state)
	}
	return out.String()
}
