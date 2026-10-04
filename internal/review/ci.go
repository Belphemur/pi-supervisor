package review

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-github/v90/github"
	"github.com/shurcooL/githubv4"
)

// CICheck is one check run observed on the PR's head sha.
type CICheck struct {
	Name       string `json:"name"`
	Status     string `json:"status"` // queued|in_progress|completed
	Conclusion string `json:"conclusion"`
	// Required marks a check the PR's protection rules demand. ONLY a required
	// check is blocking: an optional check that fails is reported but must not
	// hold a review campaign open for its whole budget (ADR-0012 §8, Q8).
	Required bool `json:"required"`
}

// CIRollup is the CI verdict for one head sha.
type CIRollup struct {
	// Verdict is pass|pending|fail and considers REQUIRED checks only.
	Verdict string `json:"verdict"`
	HeadSHA string `json:"head_sha,omitempty"`
	// Blocking is the subset that actually gates the campaign: a failing or
	// still-running required check.
	Blocking []CICheck `json:"blocking,omitempty"`
	// NonBlocking lists failing OPTIONAL checks: reported so the operator can
	// see them, deliberately excluded from Verdict.
	NonBlocking []CICheck `json:"non_blocking,omitempty"`
	// All is every check observed, required or not.
	All []CICheck `json:"checks,omitempty"`
	// RequiredUnknown is set when GitHub would not report required-ness. The
	// verdict then treats EVERY check as required, which is the safe direction:
	// it can delay a merge, never wave a red required check through.
	RequiredUnknown bool `json:"required_unknown,omitempty"`
}

// CheckCI returns the CI verdict for the PR's CURRENT head sha.
//
// Two properties make this trustworthy where the old `gh pr checks` rollup was
// not (ADR-0012 §3.3):
//
//   - It reads the Actions run's /jobs endpoint rather than a checks rollup,
//     because the rollup races state transitions right after a push and
//     transiently reads green with jobs still queued.
//   - It asks GitHub which checks the PR's protection rules REQUIRE
//     (`isRequired(pullRequestId:)`), because the jobs endpoint carries no such
//     flag and inferring it from a job name would be guesswork.
//
// Scoping to the head sha observed at poll time also prevents a verdict for the
// wrong commit when a push lands mid-poll.
func (c *Client) CheckCI(ctx context.Context, owner, repo string, pr int) (*CIRollup, error) {
	head, err := c.PRHeadSHA(ctx, owner, repo, pr)
	if err != nil {
		return nil, err
	}

	runs, _, err := c.rest.Actions.ListRepositoryWorkflowRuns(ctx, owner, repo, &github.ListWorkflowRunsOptions{
		HeadSHA: head,
		PerPage: 10,
	})
	if err != nil {
		return nil, classifyGH(restErr(err))
	}
	// No run yet is "nothing has run", never green: report pending so the
	// campaign keeps waiting instead of falsely declaring readiness.
	if runs == nil || len(runs.WorkflowRuns) == 0 {
		return &CIRollup{HeadSHA: head, Verdict: "pending"}, nil
	}

	var checks []CICheck
	for _, run := range runs.WorkflowRuns {
		jobs, _, err := c.rest.Actions.ListWorkflowJobs(ctx, owner, repo, run.GetID(), &github.ListWorkflowJobsOptions{
			PerPage: 100,
		})
		if err != nil {
			return nil, classifyGH(restErr(err))
		}
		if jobs == nil || len(jobs.Jobs) == 0 {
			continue
		}
		for _, j := range jobs.Jobs {
			checks = append(checks, CICheck{
				Name:       j.GetName(),
				Status:     j.GetStatus(),
				Conclusion: j.GetConclusion(),
			})
		}
	}
	if len(checks) == 0 {
		return &CIRollup{HeadSHA: head, Verdict: "pending"}, nil
	}

	required, known, err := c.requiredChecks(ctx, owner, repo, head, pr)
	if err != nil {
		// Could not determine required-ness: fall back to "everything blocks".
		// Failing closed here costs rounds; guessing wrong the other way lets a
		// red required check through as a pass.
		return rollup(head, checks, nil, false), nil
	}
	return rollup(head, checks, required, known), nil
}

// requiredChecks maps check-run names on the head sha to whether the PR's
// protection rules require them. ok=false means GitHub would not tell us.
func (c *Client) requiredChecks(ctx context.Context, owner, repo, sha string, pr int) (map[string]bool, bool, error) {
	var q struct {
		Repository struct {
			PullRequest struct {
				Commit struct {
					CheckSuites struct {
						Nodes []struct {
							CheckRuns struct {
								Nodes []struct {
									Name       githubv4.String
									IsRequired func(int) githubv4.Boolean
								}
							} `graphql:"checkRuns(first: 100)"`
						}
					} `graphql:"checkSuites(first: 50)"`
				} `graphql:"commit(oid: $oid)"`
			} `graphql:"pullRequest(number: $pr)"`
		} `graphql:"repository(owner: $owner, name: $name)"`
	}
	err := c.gql.Query(ctx, &q, map[string]any{
		"owner": githubv4.String(owner),
		"name":  githubv4.String(repo),
		"pr":    githubv4.Int(pr),
		"oid":   githubv4.String(sha),
	})
	if err != nil {
		return nil, false, classifyGH(graphqlErr(err))
	}
	out := map[string]bool{}
	for _, suite := range q.Repository.PullRequest.Commit.CheckSuites.Nodes {
		for _, run := range suite.CheckRuns.Nodes {
			out[string(run.Name)] = bool(run.IsRequired(pr))
		}
	}
	if len(out) == 0 {
		// No data: required-ness is UNKNOWN, not "nothing is required".
		return nil, false, nil
	}
	return out, true, nil
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
		// With required-ness unknown, treat everything as required (fail
		// closed): a spurious "not done" costs rounds, a missed red check
		// costs a broken merge.
		chk.Required = !known || required[chk.Name]
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

// BlockingSummary renders the blocking checks for a human/LLM message, and the
// non-blocking ones separately so an optional red check is visible but never
// mistaken for the gate.
func (r *CIRollup) BlockingSummary() string {
	if len(r.Blocking) == 0 {
		return "none"
	}
	return fmtChecks(r.Blocking)
}

// NonBlockingSummary renders the failing optional checks.
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
