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

// The two review mutations are ASYMMETRIC in their input field name, which is
// its own silent failure (ADR-0012 §3.2):
//
//	addPullRequestReviewThreadReply -> pullRequestReviewThreadId
//	resolveReviewThread             -> threadId
//
// Same PRRT_ value, different field. Written out explicitly rather than
// generated, because getting it backwards fails at runtime with a schema error
// an agent will misread.
//
// The TYPE NAMES are load-bearing too: shurcooL/graphql derives the GraphQL
// input type name from the Go type name, so these must be spelled exactly as
// GitHub spells them. A live test caught "addReplyInput isn't a defined input
// type (on $input)" — the struct was named for its role, not for GitHub's type,
// so every reply would have failed in production.

// AddPullRequestReviewThreadReplyInput is the input for
// addPullRequestReviewThreadReply. Named exactly as GitHub names it.
type AddPullRequestReviewThreadReplyInput struct {
	PullRequestReviewThreadID githubv4.ID     `json:"pullRequestReviewThreadId"`
	Body                      githubv4.String `json:"body"`
}

// ResolveReviewThreadInput is the input for resolveReviewThread.
type ResolveReviewThreadInput struct {
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
	in := AddPullRequestReviewThreadReplyInput{
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
	if err := c.gql.Mutate(ctx, &m, ResolveReviewThreadInput{ThreadID: githubv4.ID(threadID)}, nil); err != nil {
		return classifyGH(graphqlErr(err))
	}
	return nil
}

// UnresolveReviewThreadInput is the input for unresolveReviewThread. The type
// name must match GitHub's exactly: shurcooL/graphql derives the input type from
// the Go type name.
type UnresolveReviewThreadInput struct {
	ThreadID githubv4.ID `json:"threadId"`
}

// UnresolveThread re-opens a resolved review thread.
//
// The daemon never needs this to gate a campaign — but the live tests do, to
// leave a PR exactly as they found it after probing a resolve. Keeping it here
// rather than in the test means the live test exercises the real client rather
// than a hand-rolled mutation.
func (c *Client) UnresolveThread(ctx context.Context, threadID string) error {
	if !ValidThreadID(threadID) {
		return refuse(ReasonUnknownThread, "thread_id %q is not a %s… review-thread id", threadID, threadIDPrefix)
	}
	var m struct {
		UnresolveReviewThread struct {
			Thread struct {
				IsResolved githubv4.Boolean
			}
		} `graphql:"unresolveReviewThread(input: $input)"`
	}
	if err := c.gql.Mutate(ctx, &m, UnresolveReviewThreadInput{ThreadID: githubv4.ID(threadID)}, nil); err != nil {
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
		Body: new(body),
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
	if abuse, ok := errors.AsType[*github.AbuseRateLimitError](err); ok {
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

// parsePRURL extracts owner, repo and number from a github PR URL. Used to
// derive the review target from the already-scraped pr_url (ADR-0006) rather
// than a second scrape.
func parsePRURL(raw string) (owner, repo string, pr int, ok bool) {
	_, after, ok := strings.Cut(raw, "github.com/")
	if !ok {
		return "", "", 0, false
	}
	rest := strings.Trim(after, "/")
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
