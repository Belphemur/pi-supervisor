package supervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pi-supervisor/internal/job"
	"pi-supervisor/internal/stall"
)

// gitWorktree turns dir into a git repo carrying the given GitHub remotes
// (first entry becomes origin). No fetch, no network: `git remote get-url`
// serves the URL straight from config, which is all ADR-0023 validation needs.
func gitWorktree(t *testing.T, dir string, remotes ...string) {
	t.Helper()
	if len(remotes) == 0 {
		t.Fatal("gitWorktree needs at least one remote")
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		// git exports GIT_DIR (and friends) into hook environments — this
		// test may RUN under pre-push — and an inherited GIT_DIR redirects
		// `git init`/`git remote add` at the REAL repo. Scrub every GIT_*
		// variable so the commands see only the temp dir.
		cmd.Env = envWithoutGitVars()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	for i, u := range remotes {
		name := "origin"
		if i > 0 {
			name = "remote" + string(rune('0'+i))
		}
		run("remote", "add", name, u)
	}
}

// prGateFixture is prFixture with a worktree that carries a GitHub remote, so
// the ADR-0023 gate has something to validate against.
func prGateFixture(t *testing.T, remotes ...string) (*Supervisor, *runner) {
	t.Helper()
	testEnv(t)

	dir := t.TempDir()
	gitWorktree(t, dir, remotes...)
	sess := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sess, []byte(`{"content":"round 1 started"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	j := job.Job{
		Name: "prgate", Brief: filepath.Join(dir, "brief.md"),
		Worktree: dir, SessionName: "prgate", SessionPath: sess,
		MaxRounds: 2, TimeoutS: 60, CIStallCap: 1, CIStallIdleS: 1,
	}
	if err := os.WriteFile(j.Brief, []byte("PR GATE TEST"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	return s, s.jobs["prgate"]
}

// orchestratorLog reads the job's orchestrator log (where logf appends).
func orchestratorLog(t *testing.T, name string) string {
	t.Helper()
	_, _, orchlog, _ := job.Paths(name)
	b, err := os.ReadFile(orchlog)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(b)
}

// ADR-0023: a scraped PR URL whose owner/repo does not match one of the
// worktree's GitHub remotes is DISCARDED — never stored on state.PRURL, never
// logged. The live defect (addictedproxy-llm-seo2): the transcript referenced
// nuxt/nuxt#36542 while the campaign targeted AddictedProxy PR 3237, and the
// scraped URL's owner/repo won, arming nuxt/nuxt#3237 — a chimera.
func TestRecordPRRejectsForeignRepoURL(t *testing.T) {
	s, r := prGateFixture(t, "https://github.com/owner/project.git")

	foreign := "https://github.com/nuxt/nuxt/pull/36542"
	s.recordPR(r, foreign)

	r.mu.Lock()
	got := r.state.PRURL
	r.mu.Unlock()
	if got != "" {
		t.Fatalf("foreign URL latched: state.PRURL = %q, want empty", got)
	}
	if line := "pull request: " + foreign; strings.Contains(orchestratorLog(t, "prgate"), line) {
		t.Fatalf("foreign URL was logged: %q", line)
	}

	// A matching URL is accepted: state + log, the ADR-0006 surface unchanged.
	matching := "https://github.com/owner/project/pull/7"
	s.recordPR(r, matching)
	r.mu.Lock()
	got = r.state.PRURL
	r.mu.Unlock()
	if got != matching {
		t.Fatalf("matching URL rejected: state.PRURL = %q, want %q", got, matching)
	}
	if !strings.Contains(orchestratorLog(t, "prgate"), "pull request: "+matching) {
		t.Fatal("matching URL was not logged")
	}
}

// "One of the worktree's GitHub remotes" — not just origin. A fork workflow
// (ADR-0012) legitimately carries a second remote, and a PR link pointing at
// it is real work the agent did in this worktree.
func TestRecordPRAcceptsSecondaryRemoteURL(t *testing.T) {
	s, r := prGateFixture(t,
		"https://github.com/owner/project.git",
		"https://github.com/forker/project.git")

	link := "https://github.com/forker/project/pull/12"
	s.recordPR(r, link)
	r.mu.Lock()
	got := r.state.PRURL
	r.mu.Unlock()
	if got != link {
		t.Fatalf("secondary-remote URL rejected: state.PRURL = %q, want %q", got, link)
	}
}

// The chimera regression: a scraped URL from a foreign repo must not decide
// owner/repo — the worktree's git remote is the authority (ADR-0023). The URL
// contributes only the number when it matches; --pr overrides the number.
func TestReviewTargetRepoComesFromRemoteNotScrapedURL(t *testing.T) {
	dir := t.TempDir()
	gitWorktree(t, dir, "https://github.com/owner/project.git")

	o, rp, n, ok, divergent := reviewTarget(dir, "https://github.com/nuxt/nuxt/pull/36542", 3237)
	if !ok || o != "owner" || rp != "project" || n != 3237 {
		t.Fatalf("reviewTarget = %s/%s#%d ok=%v, want owner/project#3237 (not nuxt/nuxt#3237)", o, rp, n, ok)
	}
	if !divergent {
		t.Fatal("a foreign URL must be reported as divergent")
	}

	// A matching URL supplies the number, and the divergence flag stays quiet.
	o, rp, n, ok, divergent = reviewTarget(dir, "https://github.com/owner/project/pull/77", 0)
	if !ok || o != "owner" || rp != "project" || n != 77 || divergent {
		t.Fatalf("matching URL: reviewTarget = %s/%s#%d ok=%v divergent=%v, want owner/project#77", o, rp, n, ok, divergent)
	}

	// --pr still overrides the number of a matching URL.
	_, _, n, ok, _ = reviewTarget(dir, "https://github.com/owner/project/pull/77", 200)
	if !ok || n != 200 {
		t.Fatalf("--pr override: number = %d ok=%v, want 200/true", n, ok)
	}
}

// The URL number is adopted only when the URL names the SAME repo
// reviewTarget returns. A link to a DIFFERENT remote's repo must not donate
// its number to the primary repo — that is the chimera shape again, one
// remote over.
func TestReviewTargetRejectsNumberFromOtherRepoURL(t *testing.T) {
	dir := t.TempDir()
	gitWorktree(t, dir,
		"https://github.com/owner/project.git",
		"https://github.com/forker/project.git")

	// Stored (acceptPRURL passes: it matches a worktree remote), but the
	// number must not be applied to owner/project.
	o, rp, n, ok, divergent := reviewTarget(dir, "https://github.com/forker/project/pull/12", 0)
	if ok {
		t.Fatalf("number from a different repo's URL leaked: %s/%s#%d ok=%v", o, rp, n, ok)
	}
	if !divergent {
		t.Fatal("a URL for another remote's repo is still divergent")
	}
}

// No GitHub remote on the worktree: the campaign cannot be aimed at all
// (ADR-0023 rule 1) — even with a scraped URL, which is exactly the trust the
// rule removes.
func TestReviewTargetWithoutRemoteIsNotOK(t *testing.T) {
	dir := t.TempDir() // not a git repo
	if _, _, _, ok, _ := reviewTarget(dir, "https://github.com/owner/project/pull/7", 0); ok {
		t.Fatal("a scraped URL must not aim a campaign without a worktree remote")
	}
	if _, _, _, ok, _ := reviewTarget("", "", 43); ok {
		t.Fatal("reviewTarget invented a target with no URL and no worktree")
	}
}

// run a git command against dir with GIT_* scrubbed so an inherited GIT_DIR
// (this test can run under pre-push) cannot redirect it at the real repo.
func gitDir(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = envWithoutGitVars()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// appendSession mirrors stall.writePRLines (append-only) from the supervisor
// package, which cannot see the unexported helper.
func appendSession(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// ADR-0023: GitHub owner/repo are case-insensitive. A remote cloned as
// Belphemur/pi-supervisor must accept a scraped link citing belphemur/
// pi-supervisor (and vice-versa) — the two casings of one URL must not be
// dropped as a "chimera" of a different repo.
func TestRecordPRAcceptsCaseInsensitiveRemoteMatch(t *testing.T) {
	s, r := prGateFixture(t, "https://github.com/Belphemur/pi-supervisor.git")
	url := "https://github.com/belphemur/pi-supervisor/pull/7"
	if !s.recordPR(r, url) {
		t.Fatal("recordPR rejected a case-insensitive remote match (ADR-0023)")
	}
	r.mu.Lock()
	got := r.state.PRURL
	r.mu.Unlock()
	if got != url {
		t.Fatalf("PRURL = %q, want %q", got, url)
	}
}

// recordPR short-circuits on a URL already stored, so the polled watchdog
// (which rescrapes every tick) does NOT fork `git remote` on every round trip.
// Proved by breaking the worktree's remote after the first call: a
// non-short-circuited call would now fail acceptPRURL, but the store must still
// return true without running git.
func TestRecordPRShortCircuitsStoredURL(t *testing.T) {
	s, r := prGateFixture(t, "https://github.com/owner/project.git")
	url := "https://github.com/owner/project/pull/7"
	if !s.recordPR(r, url) {
		t.Fatal("first recordPR rejected a matching URL")
	}
	gitDir(t, r.job.Worktree, "remote", "remove", "origin") // worktree now has no remote
	if !s.recordPR(r, url) {
		t.Fatal("recordPR re-ran git on a stored URL instead of short-circuiting")
	}
}

// ADR-0023 threads: a NEWER foreign link must not mask an earlier matching one
// in the same read. recordScrapedPR re-pulls after a rejection, and the Scanner
// rewinds its read window on skip, so the surviving link latches within the same
// gate pass — not on the next tick.
func TestRecordScrapedPRLoopsToSurvivingLink(t *testing.T) {
	s, r := prGateFixture(t, "https://github.com/owner/project.git")
	// #7 (matching) precedes nuxt/36542 (foreign); the backward scan latches the
	// newest (nuxt) first, the gate rejects it, the rewind recovers #7.
	const line = `{"content":"opened https://github.com/owner/project/pull/7 while reading https://github.com/nuxt/nuxt/pull/36542"}`
	appendSession(t, r.job.SessionPath, line)

	sc := stall.NewPRURLScanner(r.job.SessionPath)
	s.recordScrapedPR(r, sc)

	r.mu.Lock()
	got := r.state.PRURL
	r.mu.Unlock()
	want := "https://github.com/owner/project/pull/7"
	if got != want {
		t.Fatalf("recordScrapedPR lost the surviving link: state.PRURL = %q, want %q", got, want)
	}
}
