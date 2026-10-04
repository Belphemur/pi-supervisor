package review

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/shurcooL/githubv4"
)

// threadIDPrefix is the GraphQL review-thread node-id prefix. Every
// thread_id on this surface is PRRT_… (ADR-0012 §3.1).
//
// This is the single most common way to waste a review turn: the numeric
// `id` field from GET /pulls/<n>/comments is a COMMENT node id, and both
// mutations reject it with "Could not resolve to a node with the global id".
// An LLM handed that field will use it. So the daemon validates the shape
// itself and the shim never has to construct one.
const threadIDPrefix = "PRRT_"

// ValidThreadID reports whether id is a GraphQL review-thread node id.
func ValidThreadID(id string) bool { return strings.HasPrefix(id, threadIDPrefix) }

// Thread is one review thread as the shim sees it.
type Thread struct {
	ThreadID   string `json:"thread_id"`
	Resolved   bool   `json:"resolved"`
	LastAuthor string `json:"last_author,omitempty"`
	Snippet    string `json:"snippet,omitempty"`
	Body       string `json:"body,omitempty"`
}

// ThreadDetail is the full, untruncated view of one thread.
type ThreadDetail struct {
	ThreadID string      `json:"thread_id"`
	Resolved bool        `json:"resolved"`
	Body     string      `json:"body,omitempty"`
	Comments []ThreadCmt `json:"comments,omitempty"`
}

// ThreadCmt is one comment in a thread. Author lives HERE, on the comment
// node — never on the review thread (ADR-0012 §3.2); selecting
// reviewThreads.nodes.author fails schema validation on every poll, which an
// agent reads as "no threads yet" and then polls forever.
type ThreadCmt struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

// threadsQuery pages reviewThreads. The shape is inherited from the proven
// query in answer-code-review's reply_review.py, not re-derived.
var threadsQuery struct {
	Repository struct {
		PullRequest struct {
			ReviewThreads struct {
				Nodes []struct {
					ID         githubv4.ID
					IsResolved githubv4.Boolean
					Comments   struct {
						Nodes []struct {
							Author struct {
								Login githubv4.String
							}
							Body githubv4.String
						}
					} `graphql:"comments(last: 1)"`
				}
				PageInfo struct {
					HasNextPage githubv4.Boolean
					EndCursor   githubv4.String
				}
			} `graphql:"reviewThreads(first: $first, after: $after)"`
		} `graphql:"pullRequest(number: $pr)"`
	} `graphql:"repository(owner: $owner, name: $name)"`
}

// threadPageSize matches the proven helper: a page of 50 silently truncated
// at 50 threads, reporting "0 open" while 17 were unresolved.
const threadPageSize = 100

// ListThreads returns every thread on the PR, paginated to EXHAUSTION.
//
// reviewThreads returns oldest-first, so a single page is mostly
// already-resolved history: stopping at page one reports "0 open threads" on a
// PR that still has dozens open (observed on PR #184 with 67 threads). A
// non-nil cursor in the RESULT therefore means "there is more" — callers that
// need the total must page.
func (c *Client) ListThreads(ctx context.Context, owner, repo string, pr int) ([]Thread, error) {
	if pr <= 0 {
		return nil, refuse(ReasonUsage, "pr must be a positive integer, got %d", pr)
	}
	var (
		out    []Thread
		cursor *githubv4.String
	)
	// Bounded so a misbehaving server cannot spin us forever; 100 pages of
	// 100 threads is far past any real PR.
	for range 100 {
		vars := map[string]any{
			"owner": githubv4.String(owner),
			"name":  githubv4.String(repo),
			"pr":    githubv4.Int(pr),
			"first": githubv4.Int(threadPageSize),
			"after": cursor,
		}
		q := threadsQuery
		if err := c.gql.Query(ctx, &q, vars); err != nil {
			return nil, classifyGH(graphqlErr(err))
		}
		conn := q.Repository.PullRequest.ReviewThreads
		for _, n := range conn.Nodes {
			t := Thread{
				ThreadID: fmt.Sprintf("%v", n.ID),
				Resolved: bool(n.IsResolved),
			}
			if len(n.Comments.Nodes) > 0 {
				last := n.Comments.Nodes[len(n.Comments.Nodes)-1]
				t.LastAuthor = string(last.Author.Login)
				clean := CleanBody(string(last.Body))
				t.Body = clean
				t.Snippet = snippet(clean)
			}
			out = append(out, t)
		}
		if !bool(conn.PageInfo.HasNextPage) {
			return out, nil
		}
		cur := conn.PageInfo.EndCursor
		cursor = &cur
	}
	return out, refuse(ReasonGitHubError, "thread pagination did not terminate")
}

// OpenThreads filters a thread list to the unresolved ones.
func OpenThreads(all []Thread) []Thread {
	out := make([]Thread, 0, len(all))
	for _, t := range all {
		if !t.Resolved {
			out = append(out, t)
		}
	}
	return out
}

// ThreadDetail returns one thread's full body and comment history.
func (c *Client) ThreadDetail(ctx context.Context, owner, repo, threadID string) (*ThreadDetail, error) {
	if !ValidThreadID(threadID) {
		return nil, refuse(ReasonUnknownThread,
			"thread_id %q is not a GraphQL review-thread id (expected %s…); "+
				"the numeric pulls-comments id is a COMMENT node and both mutations reject it",
			threadID, threadIDPrefix)
	}
	// Thread ids are global node ids, so resolve through node(id: $id).
	//
	// The inline fragment is REQUIRED and must wrap the WHOLE selection:
	// `isResolved` lives on PullRequestReviewThread, not on the Node interface,
	// so selecting it directly off node(id:) fails with "Field 'isResolved'
	// doesn't exist on type 'Node'". Putting the fragment on the inner field
	// instead of the struct's own tag does not satisfy the query builder — a
	// live test caught both misplacements.
	var byID struct {
		Node struct {
			ReviewThread struct {
				ID         githubv4.ID
				IsResolved githubv4.Boolean
				Comments   struct {
					Nodes []struct {
						Author struct {
							Login githubv4.String
						}
						Body githubv4.String
					}
				} `graphql:"comments(last: 20)"`
			} `graphql:"... on PullRequestReviewThread"`
		} `graphql:"node(id: $id)"`
	}
	err := c.gql.Query(ctx, &byID, map[string]any{"id": githubv4.ID(threadID)})
	if err != nil {
		return nil, classifyGH(graphqlErr(err))
	}
	rt := byID.Node.ReviewThread
	d := &ThreadDetail{
		ThreadID: fmt.Sprintf("%v", rt.ID),
		Resolved: bool(rt.IsResolved),
	}
	for _, cm := range rt.Comments.Nodes {
		body := string(cm.Body)
		d.Comments = append(d.Comments, ThreadCmt{Author: string(cm.Author.Login), Body: body})
		if d.Body == "" {
			d.Body = CleanBody(body)
		}
	}
	if d.ThreadID == "" {
		return nil, refuse(ReasonUnknownThread,
			"node %q did not resolve to a review thread (wrong id type, or no access)", threadID)
	}
	return d, nil
}

// snippet trims a cleaned body to the 120-char preview the listing carries.
// The full recommendation often hides behind a <details> block, so a snippet
// is a title, not the actionable text — callers wanting the real body ask for
// thread_detail.
func snippet(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

var (
	reDetails       = regexp.MustCompile(`(?s)<details>.*?</details>`)
	reHTMLComment   = regexp.MustCompile(`(?s)<!--.*?-->`)
	reCodeBlock     = regexp.MustCompile("(?s)```.*?```")
	reAnalysisChain = regexp.MustCompile(`🧩 Analysis chain`)
	reScriptExec    = regexp.MustCompile(`(?m)🏁 Script executed.*`)
	reHeaderLine    = regexp.MustCompile(`(?m)^_+.*?_+\s*\|\s*_+.*?_+\s*\|\s*_+.*?_+\s*`)
)

// CleanBody strips the noisiest CodeRabbit markup so the actionable text
// survives. Inherited from answer-code-review's proven cleaner: the bot wraps
// its real recommendation above a <details> analysis chain, with script runs
// and web searches inside, so an un-cleaned body is mostly noise.
//
// DRY: this is the ONE copy of these patterns in the tree. The Python helper
// is retired as a dependency; its rules live here instead of being re-derived.
func CleanBody(body string) string {
	if body == "" {
		return ""
	}
	body = reDetails.ReplaceAllString(body, "")
	body = reHTMLComment.ReplaceAllString(body, "")
	body = reCodeBlock.ReplaceAllString(body, "")
	body = reAnalysisChain.ReplaceAllString(body, "")
	body = reScriptExec.ReplaceAllString(body, "")
	body = reHeaderLine.ReplaceAllString(body, "")
	body = regexp.MustCompile(`\n{3,}`).ReplaceAllString(body, "\n\n")
	return strings.TrimSpace(body)
}
