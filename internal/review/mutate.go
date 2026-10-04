package review

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-github/v90/github"
	"github.com/shurcooL/githubv4"
)

// The two review mutations are ASYMMETRIC in their input field name, which is
// its own silent failure (ADR-0012 §3.2):
//
//	addPullRequestReviewThreadReply -> pullRequestReviewThreadId
//	resolveReviewThread             -> threadId
//
// Same PRRT_ value, different field. Written out explicitly here rather than
// generated, because getting it backwards fails at runtime with a schema error
// an agent will misread.

// addReplyInput is the input for addPullRequestReviewThreadReply.
type addReplyInput struct {
	PullRequestReviewThreadID githubv4.ID     `json:"pullRequestReviewThreadId"`
	Body                      githubv4.String `json:"body"`
}

// resolveInput is the input for resolveReviewThread.
type resolveInput struct {
	ThreadID githubv4.ID `json:"threadId"`
}

// PostReply adds one comment to a review thread (does NOT resolve it).
func (c *Client) PostReply(ctx context.Context, threadID, body string) error {
	if !ValidThreadID(threadID) {
		return refuse(ReasonUnknownThread, "thread_id %q is not a %s… review-thread id", threadID, threadIDPrefix)
	}
	if strings.TrimSpace(body) == "" {
		return refuse(ReasonUsage, "reply body is empty — a bare close/reply carries no record of why")
	}
	var m struct {
		AddPullRequestReviewThreadReply struct {
			Comment struct {
				ID githubv4.ID
			}
		} `graphql:"addPullRequestReviewThreadReply(input: $input)"`
	}
	in := addReplyInput{
		PullRequestReviewThreadID: githubv4.ID(threadID),
		Body:                      githubv4.String(body),
	}
	if err := c.gql.Mutate(ctx, &m, in, nil); err != nil {
		return classifyGH(graphqlErr(err))
	}
	return nil
}

// ResolveThread marks one review thread resolved. Re-resolving an already
// resolved thread is a safe no-op on GitHub's side.
func (c *Client) ResolveThread(ctx context.Context, threadID string) error {
	if !ValidThreadID(threadID) {
		return refuse(ReasonUnknownThread, "thread_id %q is not a %s… review-thread id", threadID, threadIDPrefix)
	}
	var m struct {
		ResolveReviewThread struct {
			Thread struct {
				IsResolved githubv4.Boolean
			}
		} `graphql:"resolveReviewThread(input: $input)"`
	}
	if err := c.gql.Mutate(ctx, &m, resolveInput{ThreadID: githubv4.ID(threadID)}, nil); err != nil {
		return classifyGH(graphqlErr(err))
	}
	return nil
}

// BulkResolve resolves each id, returning the ones that FAILED. Partial
// application is reported rather than hidden: the caller (a campaign) needs
// to know which threads stayed open.
//
// This is only ever called after a peer ACK (ADR-0012 §2.3); the ack gate
// lives in the supervisor, not here.
func (c *Client) BulkResolve(ctx context.Context, ids []string) []string {
	var failed []string
	for _, id := range ids {
		if err := c.ResolveThread(ctx, id); err != nil {
			failed = append(failed, id)
		}
	}
	return failed
}

// PostComment posts a top-level PR comment (used for the CodeRabbit trigger
// and the bulk-resolve audit trail). REST: a comment is not a review thread.
func (c *Client) PostComment(ctx context.Context, owner, repo string, pr int, body string) error {
	if strings.TrimSpace(body) == "" {
		return refuse(ReasonUsage, "comment body is empty")
	}
	_, _, err := c.rest.Issues.CreateComment(ctx, owner, repo, pr, &github.IssueComment{
		Body: github.Ptr(body),
	})
	if err != nil {
		return classifyGH(restErr(err))
	}
	return nil
}

// graphqlErr normalizes a shurcooL/graphql error. The library's error type is
// unexported, so the GraphQL message is carried through verbatim and
// classifyGH matches on the substrings GitHub uses for the two conditions that
// need a distinct response: throttling and rejection.
func graphqlErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	limited := strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "abuse detection")
	unauth := strings.Contains(lower, "bad credentials") ||
		strings.Contains(lower, "resource not accessible by integration")
	switch {
	case limited:
		return &GHError{Status: http.StatusForbidden, Message: msg, RateLimited: true}
	case unauth:
		return &GHError{Status: http.StatusUnauthorized, Message: msg}
	}
	return &GHError{Message: msg}
}

// restErr converts a go-github error (which carries an HTTP response) into a
// GHError, including the rate-limit signal that a 403 can also mean.
func restErr(err error) error {
	var rerr *github.ErrorResponse
	if errors.As(err, &rerr) && rerr.Response != nil {
		status := rerr.Response.StatusCode
		// A 403 with no rate-limit header is a permissions failure; with
		// x-ratelimit-remaining: 0 it is throttling. Only the former needs a
		// human, so the header is what separates them.
		limited := status == http.StatusForbidden &&
			rerr.Response.Header.Get("x-ratelimit-remaining") == "0"
		return &GHError{Status: status, Message: rerr.Message, RateLimited: limited}
	}
	var abuse *github.AbuseRateLimitError
	if errors.As(err, &abuse) {
		return &GHError{Status: http.StatusForbidden, Message: abuse.Message, RateLimited: true}
	}
	return err
}

// PRHeadSHA returns the PR's current head commit sha (REST metadata).
// go-github v90 renamed the Pulls service to PullRequests.
func (c *Client) PRHeadSHA(ctx context.Context, owner, repo string, pr int) (string, error) {
	p, _, err := c.rest.PullRequests.Get(ctx, owner, repo, pr)
	if err != nil {
		return "", classifyGH(restErr(err))
	}
	return p.GetHead().GetSHA(), nil
}

// PROpen reports whether the PR is open (state "open").
func (c *Client) PROpen(ctx context.Context, owner, repo string, pr int) (bool, error) {
	p, _, err := c.rest.PullRequests.Get(ctx, owner, repo, pr)
	if err != nil {
		return false, classifyGH(restErr(err))
	}
	return p.GetState() == "open", nil
}

// CICheck is one job's verdict within an Actions run.
type CICheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // queued|in_progress|completed
	// Conclusion mirrors GitHub's own job-conclusion vocabulary verbatim,
	// including its British spelling — matching the wire format is the point.
	Conclusion string `json:"conclusion"` //nolint:misspell // GitHub's own spelling, not ours
}

// CIRollup is the CI verdict for one head sha.
type CIRollup struct {
	Verdict string    `json:"verdict"` // pass|pending|fail
	HeadSHA string    `json:"head_sha,omitempty"`
	Checks  []CICheck `json:"checks,omitempty"`
}

// CheckCI returns the CI verdict for the PR's CURRENT head sha.
//
// It reads the Actions run's /jobs endpoint, NOT a `gh pr checks` rollup: that
// rollup races state transitions right after a push and transiently reads
// green with jobs still queued (ADR-0012 §3.3). Scoping to the head sha
// observed at poll time also prevents a verdict for the wrong commit when a
// push lands mid-poll.
func (c *Client) CheckCI(ctx context.Context, owner, repo string, pr int) (*CIRollup, error) {
	head, err := c.PRHeadSHA(ctx, owner, repo, pr)
	if err != nil {
		return nil, err
	}
	runs, _, err := c.rest.Actions.ListRepositoryWorkflowRuns(ctx, owner, repo, &github.ListWorkflowRunsOptions{
		HeadSHA: head,
		ListOptions: github.ListOptions{
			PerPage: 10,
		},
	})
	if err != nil {
		return nil, classifyGH(restErr(err))
	}
	out := &CIRollup{HeadSHA: head, Verdict: "pass"}
	// No run yet: not "green", it is "nothing has run". Report pending so the
	// campaign keeps waiting instead of falsely declaring readiness.
	if runs == nil || len(runs.WorkflowRuns) == 0 {
		out.Verdict = "pending"
		return out, nil
	}
	fail, pending := false, false
	for _, run := range runs.WorkflowRuns {
		jobs, _, err := c.rest.Actions.ListWorkflowJobs(ctx, owner, repo, run.GetID(), &github.ListWorkflowJobsOptions{
			ListOptions: github.ListOptions{PerPage: 100},
		})
		if err != nil {
			return nil, classifyGH(restErr(err))
		}
		if jobs == nil || len(jobs.Jobs) == 0 {
			pending = true
			continue
		}
		for _, j := range jobs.Jobs {
			chk := CICheck{Name: j.GetName(), Status: j.GetStatus(), Conclusion: j.GetConclusion()}
			out.Checks = append(out.Checks, chk)
			switch j.GetConclusion() {
			case "success", "neutral", "skipped", "":
				if j.GetStatus() != "completed" {
					pending = true
				}
			default:
				fail = true
			}
		}
	}
	switch {
	case fail:
		out.Verdict = "fail"
	case pending:
		out.Verdict = "pending"
	}
	return out, nil
}

// parsePRURL extracts owner, repo and number from a github PR URL. Used to
// derive the review target from the already-scraped pr_url (ADR-0006) rather
// than a second scrape.
func parsePRURL(raw string) (owner, repo string, pr int, ok bool) {
	i := strings.Index(raw, "github.com/")
	if i < 0 {
		return "", "", 0, false
	}
	rest := strings.Trim(raw[i+len("github.com/"):], "/")
	parts := strings.Split(rest, "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return "", "", 0, false
	}
	var n int
	if _, err := fmt.Sscanf(parts[3], "%d", &n); err != nil || n <= 0 {
		return "", "", 0, false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), n, true
}

// ParsePRURL is the exported form used by the supervisor's auto-trigger.
func ParsePRURL(raw string) (owner, repo string, pr int, ok bool) {
	return parsePRURL(raw)
}
