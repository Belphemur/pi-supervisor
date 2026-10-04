package review

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Live GitHub tests. Off by default; enable with:
//
//	PI_SUPERVISOR_LIVE_GH=1 go test ./internal/review/ -run Live -v
//
// They are READ-ONLY on purpose: they list threads, read CI, and fetch thread
// bodies. Nothing here posts, replies, or resolves — a test must never mutate a
// real PR to prove a read works.
//
// The repo/PR come from the environment so the test can be pointed at any
// target the caller trusts:
//
//	PI_SUPERVISOR_LIVE_REPO=Belphemur/XPoint
//	PI_SUPERVISOR_LIVE_PR=171
func liveConfig(t *testing.T) (owner, repo string, pr int) {
	t.Helper()
	if os.Getenv("PI_SUPERVISOR_LIVE_GH") == "" {
		t.Skip("set PI_SUPERVISOR_LIVE_GH=1 to run live GitHub tests (read-only)")
	}
	repoRef := os.Getenv("PI_SUPERVISOR_LIVE_REPO")
	if repoRef == "" {
		t.Skip("set PI_SUPERVISOR_LIVE_REPO=owner/name")
	}
	prStr := os.Getenv("PI_SUPERVISOR_LIVE_PR")
	if prStr == "" {
		t.Skip("set PI_SUPERVISOR_LIVE_PR=<number>")
	}
	for i := 0; i < len(repoRef); i++ {
		if repoRef[i] == '/' {
			return repoRef[:i], repoRef[i+1:], atoi(t, prStr)
		}
	}
	t.Fatalf("PI_SUPERVISOR_LIVE_REPO=%q is not owner/name", repoRef)
	return "", "", 0
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("PI_SUPERVISOR_LIVE_PR=%q is not a number", s)
		}
		n = n*10 + int(r-'0')
	}
	if n <= 0 {
		t.Fatalf("PI_SUPERVISOR_LIVE_PR=%q must be positive", s)
	}
	return n
}

func liveClient(t *testing.T) (*Client, string, string, int) {
	t.Helper()
	owner, repo, pr := liveConfig(t)
	// Deliberately no GITHUB_APP_ID: the fallback path (`gh auth token`) is
	// what this exercises, and it is the path local review jobs use.
	t.Setenv("GITHUB_APP_ID", "")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := NewClient(ctx, owner, repo)
	if err != nil {
		t.Fatalf("NewClient (gh auth token fallback): %v", err)
	}
	return c, owner, repo, pr
}

// LiveListThreads proves the real GraphQL query shape works — the query is
// inherited from a proven helper, but "inherited" is not "verified against
// GitHub today", and a schema change would surface only here.
func TestLiveListThreads(t *testing.T) {
	c, owner, repo, pr := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	all, err := c.ListThreads(ctx, owner, repo, pr)
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	t.Logf("%s/%s#%d: %d thread(s), %d open", owner, repo, pr, len(all), len(OpenThreads(all)))
	if len(all) == 0 {
		t.Fatal("a PR with review history returned no threads — the query shape is wrong")
	}
	// Every id must be a PRRT_ node id: that is the invariant the whole
	// surface rests on, and it is only provable against real data.
	for _, th := range all {
		if !ValidThreadID(th.ThreadID) {
			t.Fatalf("GitHub returned a non-PRRT thread id %q — the protocol is wrong", th.ThreadID)
		}
	}
	// Bodies must be cleaned: a CodeRabbit thread's raw body is almost entirely
	// analysis-chain noise, so an uncleaned body means CleanBody is not running.
	var coderabbit int
	for _, th := range all {
		if th.LastAuthor == "coderabbitai" {
			coderabbit++
			if strings.Contains(th.Body, "Analysis chain") {
				t.Errorf("thread %s kept its analysis chain after cleaning", th.ThreadID)
			}
			if th.Snippet == "" {
				t.Errorf("thread %s has an empty snippet", th.ThreadID)
			}
		}
	}
	t.Logf("cleaned %d coderabbitai thread(s)", coderabbit)
}

// LiveThreadDetail proves node(id:) resolution works for a PRRT_ id.
func TestLiveThreadDetail(t *testing.T) {
	c, owner, repo, pr := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	all, err := c.ListThreads(ctx, owner, repo, pr)
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(all) == 0 {
		t.Skip("no threads to detail")
	}
	id := all[0].ThreadID
	d, err := c.ThreadDetail(ctx, owner, repo, id)
	if err != nil {
		t.Fatalf("ThreadDetail(%s): %v", id, err)
	}
	if d.ThreadID != id {
		t.Errorf("ThreadDetail returned %s, want %s", d.ThreadID, id)
	}
	t.Logf("thread %s: resolved=%v comments=%d body=%d chars",
		id, d.Resolved, len(d.Comments), len(d.Body))
	if len(d.Comments) == 0 {
		t.Error("a thread with comments returned none")
	}
	// Author lives on the comment node — the query must prove that works live.
	for _, c := range d.Comments {
		if c.Author == "" {
			t.Error("a comment came back with no author; the author field is on the wrong node")
		}
	}
}

// LiveCheckCI proves the required/optional split against a real PR: the
// isRequired query must return data, not an error that silently degrades to
// fail-closed.
func TestLiveCheckCI(t *testing.T) {
	c, owner, repo, pr := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	roll, err := c.CheckCI(ctx, owner, repo, pr)
	if err != nil {
		t.Fatalf("CheckCI: %v", err)
	}
	t.Logf("head=%s verdict=%s required_unknown=%v checks=%d blocking=%d non-blocking=%d",
		roll.HeadSHA, roll.Verdict, roll.RequiredUnknown, len(roll.All), len(roll.Blocking), len(roll.NonBlocking))
	switch roll.Verdict {
	case "pass", "pending", "fail":
	default:
		t.Fatalf("verdict %q is outside the closed set", roll.Verdict)
	}
	// The head sha must be a real commit id, not empty: an empty sha would make
	// "scoped to the current head" a claim with nothing behind it.
	if roll.HeadSHA == "" {
		t.Error("head_sha is empty — the verdict is not scoped to a commit")
	}
	// NonBlocking is the "FAILING but ignorable" report, so a passing check
	// must never appear in it. (On an unprotected branch every check is
	// non-required, so a repo where everything is green legitimately yields an
	// empty list — the assertion below holds either way.)
	for _, chk := range roll.NonBlocking {
		if !failing(chk.Conclusion) {
			t.Errorf("non-blocking list holds a non-failing check %s=%s", chk.Name, chk.Conclusion)
		}
	}
	// A required check that is failing must be blocking, and vice versa: the two
	// lists partition the failures.
	for _, chk := range roll.Blocking {
		if !chk.Required {
			t.Errorf("blocking list holds a non-required check %s", chk.Name)
		}
	}
	if roll.NoProtection && len(roll.Blocking) != 0 {
		t.Errorf("no branch protection means nothing is required, so nothing can block: %v", roll.Blocking)
	}
	if roll.RequiredUnknown && !roll.NoProtection {
		t.Error("a branch cannot be both unprotected and required-unknown")
	}
	t.Logf("base=%s no_protection=%v required_unknown=%v", roll.BaseBranch, roll.NoProtection, roll.RequiredUnknown)
}

// LivePROpen proves the PR-state read used by the auto-trigger works.
func TestLivePROpen(t *testing.T) {
	c, owner, repo, pr := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sha, err := c.PRHeadSHA(ctx, owner, repo, pr)
	if err != nil {
		t.Fatalf("PRHeadSHA: %v", err)
	}
	open, err := c.PROpen(ctx, owner, repo, pr)
	if err != nil {
		t.Fatalf("PROpen: %v", err)
	}
	t.Logf("%s/%s#%d open=%v head=%s", owner, repo, pr, open, sha)
	if sha == "" {
		t.Error("head sha empty")
	}
}
