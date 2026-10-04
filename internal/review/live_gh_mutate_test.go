package review

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Live MUTATION tests. Opt-in, and they DO write to the PR: replies are posted
// and threads are resolved.
//
//	PI_SUPERVISOR_LIVE_GH=1 PI_SUPERVISOR_LIVE_MUTATE=1 \
//	PI_SUPERVISOR_LIVE_REPO=Belphemur/XPoint PI_SUPERVISOR_LIVE_PR=171 \
//	  go test ./internal/review/ -run LiveMutate -v
//
// The separate PI_SUPERVISOR_LIVE_MUTATE gate is deliberate: reading a PR is
// harmless, posting to it is not, and nobody should mutate a PR by leaving
// PI_SUPERVISOR_LIVE_GH set.
//
// Two safety properties, so a run cannot silently damage real work:
//
//   - Every reply is prefixed with probeMarker, so a human can find it.
//   - Every thread the test resolves is RE-OPENED afterwards, so the PR is left
//     in the state it was found. Run with -v to see the cleanup lines.
const probeMarker = "[pi-supervisor live-probe]"

func liveMutateClient(t *testing.T) (*Client, string, string, int) {
	t.Helper()
	if os.Getenv("PI_SUPERVISOR_LIVE_MUTATE") == "" {
		t.Skip("set PI_SUPERVISOR_LIVE_MUTATE=1 to allow live WRITES (posts replies to the PR)")
	}
	c, owner, repo, pr := liveClient(t)
	return c, owner, repo, pr
}

// anOpenThread picks a currently-OPEN thread, so the probe exercises a real
// open -> answered -> resolved transition instead of a no-op on a closed one.
func anOpenThread(t *testing.T, c *Client, owner, repo string, pr int) Thread {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	all, err := c.ListThreads(ctx, owner, repo, pr)
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	for _, th := range OpenThreads(all) {
		if ValidThreadID(th.ThreadID) {
			return th
		}
	}
	t.Skip("no OPEN thread to probe; unresolve one first with:\n" +
		"  gh api graphql -f query='mutation { unresolveReviewThread(input:{threadId:\"PRRT_…\"}) { thread { isResolved } } }'")
	return Thread{}
}

// TestLiveMutateOpenThreadRoundTrip is the real end-to-end review loop against
// GitHub: take an OPEN thread, answer it, resolve it, and assert the state
// transition at each step — then put it back.
//
// It exists because a live test found BOTH mutations broken in ways no unit
// fixture could catch: the input structs were named addReplyInput / resolveInput,
// but shurcooL/graphql derives the GraphQL input type name from the Go type
// name, so GitHub rejected them with "addReplyInput isn't a defined input type
// (on $input)". Every reply the daemon sent would have failed in production.
func TestLiveMutateOpenThreadRoundTrip(t *testing.T) {
	c, owner, repo, pr := liveMutateClient(t)
	thread := anOpenThread(t, c, owner, repo, pr)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	t.Logf("target %s: open, last author %s", thread.ThreadID, thread.LastAuthor)
	if thread.Resolved {
		t.Fatal("anOpenThread returned a resolved thread")
	}

	body := probeMarker + " answer-before-resolve round trip: posting via " +
		"addPullRequestReviewThreadReply (input `pullRequestReviewThreadId`), then resolving " +
		"via resolveReviewThread (`threadId`). No code change implied — safe to delete."
	if err := c.PostReply(ctx, thread.ThreadID, body); err != nil {
		t.Fatalf("PostReply on an open thread: %v", err)
	}
	t.Log("reply posted")

	// Read back BEFORE resolving: the reply must be visible on GitHub, not merely
	// accepted by the API.
	d, err := c.ThreadDetail(ctx, owner, repo, thread.ThreadID)
	if err != nil {
		t.Fatalf("ThreadDetail after reply: %v", err)
	}
	var landed bool
	for _, cm := range d.Comments {
		if strings.Contains(cm.Body, probeMarker) {
			landed = true
			t.Logf("reply confirmed on GitHub, author %s", cm.Author)
		}
	}
	if !landed {
		t.Fatal("the reply was accepted but is not visible on the thread")
	}
	if d.Resolved {
		t.Fatal("thread reported resolved before we resolved it")
	}

	if err := c.ResolveThread(ctx, thread.ThreadID); err != nil {
		t.Fatalf("ResolveThread: %v", err)
	}
	d2, err := c.ThreadDetail(ctx, owner, repo, thread.ThreadID)
	if err != nil {
		t.Fatalf("ThreadDetail after resolve: %v", err)
	}
	if !d2.Resolved {
		t.Fatal("thread still open after a successful resolve")
	}
	t.Log("open -> answered -> resolved verified")

	// Leave the PR as we found it.
	if err := c.UnresolveThread(ctx, thread.ThreadID); err != nil {
		t.Errorf("cleanup: could not re-open %s: %v", thread.ThreadID, err)
	} else {
		t.Logf("cleanup: %s re-opened, PR left as found", thread.ThreadID)
	}
}

// TestLiveMutateRejectsBadInput proves the guards refuse BEFORE any network
// mutation, so a confused caller cannot half-apply a change.
func TestLiveMutateRejectsBadInput(t *testing.T) {
	c, owner, repo, pr := liveMutateClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A numeric comment id — the #1 wasted-turn trap. Refused locally.
	if err := c.PostReply(ctx, "3904873498", probeMarker+" should never be sent"); err == nil {
		t.Error("PostReply accepted a numeric comment id")
	} else {
		t.Logf("PostReply(numeric id) -> %v", err)
	}
	if err := c.ResolveThread(ctx, "3904873498"); err == nil {
		t.Error("ResolveThread accepted a numeric comment id")
	} else {
		t.Logf("ResolveThread(numeric id) -> %v", err)
	}
	thread := anOpenThread(t, c, owner, repo, pr)
	// An empty body carries no record of why, so it is refused too.
	if err := c.PostReply(ctx, thread.ThreadID, "   "); err == nil {
		t.Error("PostReply accepted an empty body")
	} else {
		t.Logf("PostReply(empty body) -> %v", err)
	}
	// A bogus PRRT_ must fail loudly rather than read as an empty thread.
	bogus := threadIDPrefix + "kwDOthisIsNotARealNodeId"
	if _, err := c.ThreadDetail(ctx, owner, repo, bogus); err == nil {
		t.Error("ThreadDetail accepted a nonexistent node id")
	} else {
		t.Logf("ThreadDetail(bogus id) -> %v", err)
	}
}

// TestLiveMutateBulkResolveOverOpenThreads drives the bulk path over OPEN
// threads, verifies each state actually moved, then re-opens them all.
func TestLiveMutateBulkResolveOverOpenThreads(t *testing.T) {
	c, owner, repo, pr := liveMutateClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	all, err := c.ListThreads(ctx, owner, repo, pr)
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	var ids []string
	for _, th := range OpenThreads(all) {
		ids = append(ids, th.ThreadID)
	}
	if len(ids) == 0 {
		t.Skip("no OPEN threads to probe")
	}
	failed := c.BulkResolve(ctx, ids)
	t.Logf("BulkResolve over %d open thread(s): %d failed %v", len(ids), len(failed), failed)
	if len(failed) != 0 {
		t.Errorf("bulk resolve reported failures: %v", failed)
	}
	for _, id := range ids {
		d, err := c.ThreadDetail(ctx, owner, repo, id)
		if err != nil {
			t.Errorf("ThreadDetail(%s) after bulk resolve: %v", id, err)
			continue
		}
		if !d.Resolved {
			t.Errorf("bulk resolve reported success but %s is still open", id)
			continue
		}
		if err := c.UnresolveThread(ctx, id); err != nil {
			t.Logf("cleanup: could not re-open %s: %v", id, err)
		}
	}
	t.Logf("all %d thread(s) verified closed then re-opened; PR left as found", len(ids))
}
