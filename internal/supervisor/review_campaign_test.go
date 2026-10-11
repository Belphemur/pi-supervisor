package supervisor

import (
	"os/exec"
	"testing"
	"time"
)

// The answer-before-resolve guard is the invariant the ADR exists to enforce.
// Its sharpest edge is ROUND SCOPING: a reply from round N-1 must not
// authorize a close in round N, or a stale answer silently legitimizes a
// finding that has since been superseded.
func TestResolveGuardRequiresSameRoundReply(t *testing.T) {
	c := newReviewCampaign("o", "r", 43, 5, "acceptance")
	const tid = "PRRT_kwDOabc"

	if c.answeredInRound(3, tid) {
		t.Fatal("a fresh campaign must not consider anything answered")
	}
	c.markAnswered(3, tid)
	if !c.answeredInRound(3, tid) {
		t.Fatal("a reply in round 3 must authorize a resolve in round 3")
	}
	if c.answeredInRound(4, tid) {
		t.Fatal("a reply in round 3 must NOT authorize a resolve in round 4")
	}
	if c.answeredInRound(2, tid) {
		t.Fatal("a reply in round 3 must NOT retroactively answer round 2")
	}
}

func TestMarkAnsweredIsPerThread(t *testing.T) {
	c := newReviewCampaign("o", "r", 43, 5, "acceptance")
	c.markAnswered(1, "PRRT_a", "PRRT_b")
	if !c.answeredInRound(1, "PRRT_a") || !c.answeredInRound(1, "PRRT_b") {
		t.Fatal("both threads from the batch must be recorded")
	}
	if c.answeredInRound(1, "PRRT_c") {
		t.Fatal("an unmentioned thread must not be recorded")
	}
}

// bulk_resolve applies nothing until acked, and an unacked request must expire
// to "threads stay open" rather than wedge the campaign.
func TestPendingAckIsSingleUseAndExpires(t *testing.T) {
	c := newReviewCampaign("o", "r", 43, 5, "acceptance")
	ids := []string{"PRRT_a", "PRRT_b"}
	pa := c.addPendingAck("job", ids, "out of scope", 30*time.Minute)
	if pa.ID == "" {
		t.Fatal("an ack id must be issued so the caller can echo it")
	}
	if len(c.pendingAcks) != 1 {
		t.Fatalf("pendingAcks = %d, want 1", len(c.pendingAcks))
	}

	got, ok := c.takeAck(pa.ID)
	if !ok || len(got.ThreadIDs) != 2 {
		t.Fatalf("takeAck = %v %v", got, ok)
	}
	if _, again := c.takeAck(pa.ID); again {
		t.Fatal("an ack must be single-use: a replay must find nothing")
	}
}

func TestExpireAcksDropsOnlyExpired(t *testing.T) {
	c := newReviewCampaign("o", "r", 43, 5, "acceptance")
	now := time.Now()
	old := c.addPendingAck("job", []string{"PRRT_a"}, "stale", -time.Minute) // already past
	fresh := c.addPendingAck("job", []string{"PRRT_b"}, "current", 30*time.Minute)

	gone := c.expireAcks(now)
	if len(gone) != 1 || gone[0].ID != old.ID {
		t.Fatalf("expireAcks = %v, want only %s", gone, old.ID)
	}
	if _, still := c.takeAck(fresh.ID); !still {
		t.Fatal("a live request must survive the sweep")
	}
}

func TestShortIDIsStableEnoughToEcho(t *testing.T) {
	// The ack id is an opaque handle, not a secret; it only has to be unique
	// within a campaign and safe to pass around unquoted.
	c := newReviewCampaign("o", "r", 43, 5, "acceptance")
	a := c.addPendingAck("job", []string{"PRRT_a"}, "r1", time.Minute)
	b := c.addPendingAck("job", []string{"PRRT_b"}, "r2", time.Minute)
	if a.ID == b.ID {
		t.Fatalf("two pending acks share id %q — an ack would apply the wrong one", a.ID)
	}
	for _, id := range []string{a.ID, b.ID} {
		if id == "" {
			t.Fatal("empty ack id")
		}
		for _, r := range id {
			alnum := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
			if !alnum && r != '-' {
				t.Fatalf("ack id %q contains %q, which needs shell quoting", id, r)
			}
		}
	}
}

func TestNewReviewCampaignDefaultsRounds(t *testing.T) {
	if got := newReviewCampaign("o", "r", 1, 0, "acceptance").maxRound; got != 5 {
		t.Errorf("maxRound = %d, want the 5-round default", got)
	}
	if got := newReviewCampaign("o", "r", 1, 3, "rebuttal").maxRound; got != 3 {
		t.Errorf("maxRound = %d, want the explicit 3", got)
	}
}

// The review target comes from the already-scraped pr_url (ADR-0006), with
// git remote only as the fallback for a manual --pr on a job that never
// linked its PR.
func TestParseGitRemote(t *testing.T) {
	cases := []struct {
		in     string
		owner  string
		repo   string
		wantOK bool
	}{
		{"git@github.com:Belphemur/crosspoint-x-reader.git", "Belphemur", "crosspoint-x-reader", true},
		{"https://github.com/o/r.git", "o", "r", true},
		{"https://github.com/o/r", "o", "r", true},
		{"ssh://git@github.com/o/r.git", "o", "r", true},
		{"ssh://git@github.com:22/o/r.git", "o", "r", true},
		{"git@github.mycompany.com:o/r.git", "o", "r", true},     // GHES
		{"https://github.mycompany.com/o/r.git", "o", "r", true}, // GHES https
		{"gh:o/r.git", "", "", false},                            // bare alias, no host
		{"https://gitlab.com/o/r.git", "", "", false},
		{"https://bitbucket.org/o/r.git", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		o, rp, ok := parseGitRemote(c.in)
		if ok != c.wantOK {
			t.Errorf("parseGitRemote(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if ok && (o != c.owner || rp != c.repo) {
			t.Errorf("parseGitRemote(%q) = %s/%s, want %s/%s", c.in, o, rp, c.owner, c.repo)
		}
	}
}

// remoteRepo works for ANY repo the job's worktree sits in: origin first,
// then any other remote name (a worktree may use upstream/fork/deploy names).
func TestRemoteRepoFallsBackToNonOriginRemote(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = envWithoutGitVars()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "upstream", "git@github.com:Belphemur/pi-supervisor.git")
	o, r, ok := remoteRepo(dir)
	if !ok || o != "Belphemur" || r != "pi-supervisor" {
		t.Fatalf("remoteRepo = %s/%s ok=%v, want Belphemur/pi-supervisor", o, r, ok)
	}
	// A remote pointing at a NON-GitHub host must not win over a GitHub
	// remote: the loop keeps scanning past unparseable URLs. `git remote -v`
	// lists alphabetically (gitlab < ghnext < upstream), so ghnext is the
	// first GitHub hit here.
	run("remote", "add", "gitlab", "https://gitlab.com/o/r.git")
	run("remote", "add", "ghnext", "git@github.com:Belphemur/flambette.git")
	o, r, ok = remoteRepo(dir)
	if !ok || o != "Belphemur" || r != "flambette" {
		t.Fatalf("remoteRepo with mixed hosts = %s/%s ok=%v, want Belphemur/flambette (first GitHub remote alphabetically)", o, r, ok)
	}
}

// A scraped pr_url is authoritative: it is the PR the job actually opened, so
// it outranks a manual --pr and the worktree remote.
func TestReviewTargetPrefersScrapedURL(t *testing.T) {
	o, rp, pr, ok := reviewTarget("", "https://github.com/Belphemur/sdk/pull/184", 0)
	if !ok || o != "Belphemur" || rp != "sdk" || pr != 184 {
		t.Fatalf("reviewTarget = %s/%s#%d ok=%v", o, rp, pr, ok)
	}
	// An explicit --pr overrides the NUMBER but not the repo.
	o, rp, pr, ok = reviewTarget("", "https://github.com/Belphemur/sdk/pull/184", 200)
	if !ok || pr != 200 || rp != "sdk" {
		t.Fatalf("explicit --pr = %s/%s#%d ok=%v", o, rp, pr, ok)
	}
	// No URL and no worktree: nothing to derive.
	if _, _, _, ok := reviewTarget("", "", 43); ok {
		t.Fatal("reviewTarget invented a target with no URL and no worktree")
	}
}
