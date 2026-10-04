package supervisor

import (
	"os/exec"
	"strings"

	"pi-supervisor/internal/review"
)

// reviewTarget resolves the GitHub owner/repo/PR a review campaign targets.
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

// remoteRepo derives owner/repo from the worktree's `origin` remote, the same
// way the retired reply_review.py helper did — so a manual review works on a
// job whose agent never linked its PR.
func remoteRepo(worktree string) (owner, repo string, ok bool) {
	if worktree == "" {
		return "", "", false
	}
	out, err := exec.Command("git", "-C", worktree, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", "", false
	}
	return parseGitRemote(strings.TrimSpace(string(out)))
}

// parseGitRemote extracts owner/repo from any of the shapes git emits:
// git@github.com:owner/repo.git, https://github.com/owner/repo(.git), or an
// ssh:// URL.
func parseGitRemote(url string) (owner, repo string, ok bool) {
	if url == "" {
		return "", "", false
	}
	path := url
	if i := strings.Index(path, "github.com"); i >= 0 {
		path = path[i+len("github.com"):]
	} else {
		return "", "", false
	}
	path = strings.TrimLeft(path, ":/")
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
