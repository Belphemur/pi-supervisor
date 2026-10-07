package supervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"pi-supervisor/internal/job"
)

// originRepo drives the post-push hook's scoping, so a wrong answer makes every
// job look unrelated and the hook silently checks nothing.
//
// It shipped returning an EMPTY OWNER with the right repo, because the parser
// returned the named return `owner` (never assigned) instead of the parsed
// value. Every comparison then failed against a real owner and
// `review-recheck --pushed` reported "skipped: 31" while looking correct.
//
// The live symptom was invisible in tests because the one scoping test passed an
// explicit owner/name, bypassing originRepo entirely. This one goes through the
// real thing: a git repo with a real origin URL.
func TestOriginRepoParsesOwnerAndName(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	testEnv(t)

	cases := []struct {
		name      string
		remote    string
		wantOwner string
		wantRepo  string
	}{
		{"https", "https://github.com/Belphemur/pi-supervisor.git", "Belphemur", "pi-supervisor"},
		{"https no .git", "https://github.com/Belphemur/pi-supervisor", "Belphemur", "pi-supervisor"},
		{"ssh", "git@github.com:Belphemur/pi-supervisor.git", "Belphemur", "pi-supervisor"},
		{"ssh url form", "ssh://git@github.com/Belphemur/pi-supervisor.git", "Belphemur", "pi-supervisor"},
		{"non-github", "https://gitlab.com/o/r.git", "", ""},
		{"malformed", "https://github.com/onlyowner", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wt := t.TempDir()
			for _, args := range [][]string{
				{"init"},
				{"remote", "add", "origin", tc.remote},
			} {
				cmd := exec.Command("git", append([]string{"-C", wt}, args...)...)
				// git exports GIT_DIR (and friends) into hook environments —
				// this test RUNS under pre-push — and an inherited GIT_DIR
				// redirects `git init`/`git remote add` at the REAL repo,
				// failing with "remote origin already exists". Scrub every
				// GIT_* variable so the commands see only the temp dir.
				cmd.Env = envWithoutGitVars()
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			// The job's worktree must exist for the probe to read its remote.
			if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
				t.Skipf("git init produced no .git: %v", err)
			}
			j := job.Job{Name: "org", Brief: "b", Worktree: wt}
			writeJob(t, j)
			s := New()
			if err := s.LoadJobs(); err != nil {
				t.Fatal(err)
			}
			o, r := s.originRepo()
			if o != tc.wantOwner || r != tc.wantRepo {
				t.Fatalf("originRepo() = %q/%q, want %q/%q", o, r, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}

// A job whose baseline repo matches the pushed repo must NOT be skipped. This is
// the assertion that was missing: the scoping test passed an explicit slug, so
// the empty-owner bug slipped through even though the outcome ("everything
// skipped") was exactly what a broken parser produces.
func TestRecheckAllIncludesMatchingRepo(t *testing.T) {
	testEnv(t)
	pi := fakePiPath(t)

	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("TEST_SLOW SECS=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := job.Job{
		Name: "match", Brief: brief, Worktree: dir, SessionName: "match",
		MaxRounds: 1, TimeoutS: 60, PiBin: pi,
	}
	pinSession(t, &j, brief)
	writeJob(t, j)

	s := New()
	if err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	r := s.jobs["match"]
	s.recordThreadBaseline("match", r, "Belphemur", "pi-supervisor", 3, 0)

	res, err := s.RecheckThreadsAll(t.Context(), false, "Belphemur/pi-supervisor")
	if err != nil {
		t.Fatal(err)
	}
	m, ok := res.(recheckAllResult)
	if !ok {
		t.Fatalf("unexpected type %T", res)
	}
	if m.Skipped != 0 {
		t.Fatalf("a job whose baseline repo MATCHES was skipped (%d skipped)", m.Skipped)
	}
	if len(m.Jobs) != 1 {
		t.Fatalf("expected the job to be checked, got %+v", m.Jobs)
	}
}
