package supervisor

import (
	"net/url"
	"os/exec"
	"pi-supervisor/internal/review"
	"regexp"
	"strings"
)

// scpLike matches user@host:path (git's scp-like syntax) and bare host:path.
var scpLike = regexp.MustCompile(`^(?:[\w.-]+@)?([\w.-]+):(.+)$`)

// reviewTarget resolves the GitHub owner/repo/PR a review campaign operates on.
//
// DRY: the PR's owner and repo are read out of the ALREADY-scraped pr_url
// (ADR-0006) when the agent linked one, and only fall back to `git remote`
// for a manual `review --pr N` on a job that never linked a URL. Two sources
// for one fact would be a DRY violation; this is an ordered fallback, and the
// scraped URL always wins because it is the PR the job actually opened.
func reviewTarget(worktree, prURL string, pr int) (owner, repo string, number int, ok bool) {
	if prURL != "" {
		if o, rp, n, good := review.ParsePRURL(prURL); good {
			if pr > 0 {
				n = pr
			}
			return o, rp, n, true
		}
	}
	o, rp, good := remoteRepo(worktree)
	if !good {
		return "", "", 0, false
	}
	return o, rp, pr, pr > 0
}

// remoteRepo derives owner/repo from the worktree's git remotes — origin
// first, then any other remote. A worktree may legitimately use a different
// remote name (upstream, a fork remote, a deploy name); the review must work
// for ANY repo the job happens to sit in (owner directive 2026-10-10), so a
// non-origin name is never a dead end.
func remoteRepo(worktree string) (owner, repo string, ok bool) {
	if worktree == "" {
		return "", "", false
	}
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", append([]string{"-C", worktree}, args...)...)
		cmd.Env = envWithoutGitVars() // GIT_DIR from a hook env redirects -C at the wrong repo
		return cmd.Output()
	}
	if out, err := run("remote", "get-url", "origin"); err == nil {
		if o, r, good := parseGitRemote(strings.TrimSpace(string(out))); good {
			return o, r, true
		}
	}
	out, err := run("remote", "-v")
	if err != nil {
		return "", "", false
	}
	seen := map[string]bool{}
	for line := range strings.SplitSeq(string(out), "\n") {
		// "name\turl (fetch)" — parse both fetch and push, dedupe by URL.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		u := fields[1]
		if seen[u] {
			continue
		}
		seen[u] = true
		if o, r, good := parseGitRemote(u); good {
			return o, r, true
		}
	}
	return "", "", false
}

// parseGitRemote extracts owner/repo from any of the shapes git emits:
// git@github.com:owner/repo.git, https://github.com/owner/repo(.git), an
// ssh:// URL (with optional port), scp-like syntax, and GitHub Enterprise
// Server hosts (any hostname containing "github", e.g.
// github.mycompany.com). A host with no github marker is not a GitHub
// remote — the review surface is GitHub-only, so failing here is correct.
func parseGitRemote(raw string) (owner, repo string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}

	// URL forms: https://, ssh://, git:// — parse with net/url. The scp-like
	// normalization applies ONLY when there is no scheme: "https://..." also
	// matches host:path with host="https" (two colons), and rewriting it
	// corrupts the URL.
	normalized := raw
	if !strings.Contains(raw, "://") {
		if m := scpLike.FindStringSubmatch(raw); m != nil {
			normalized = "ssh://" + m[1] + "/" + m[2]
		}
	}
	if strings.Contains(normalized, "://") {
		u, err := url.Parse(normalized)
		if err == nil && u.Host != "" {
			host := u.Hostname()
			if !isGitHubHost(host) {
				return "", "", false
			}
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) >= 2 && parts[0] != "" {
				return parts[0], strings.TrimSuffix(parts[1], ".git"), true
			}
			return "", "", false
		}
	}

	// Bare host:path without a scheme (git@github.com:owner/repo.git).
	if m := scpLike.FindStringSubmatch(raw); m != nil {
		if !isGitHubHost(m[1]) {
			return "", "", false
		}
		path := strings.TrimSuffix(m[2], ".git")
		parts := strings.Split(path, "/")
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			return parts[0], parts[1], true
		}
	}
	return "", "", false
}

// isGitHubHost: github.com or any GitHub Enterprise host (hostname contains
// "github"). Anything else (gitlab, bitbucket, a bare IP) is not a target —
// the review verbs are GitHub mutations.
func isGitHubHost(host string) bool {
	h := strings.ToLower(host)
	return h == "github.com" || strings.Contains(h, "github")
}
