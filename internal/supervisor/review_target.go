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
// ADR-0023: the worktree's git remote is the AUTHORITY for owner/repo — a
// transcript link is untrusted (the agent may cite a PR it merely read; the
// addictedproxy-llm-seo2 campaign armed nuxt/nuxt#3237 from a scraped nuxt
// URL plus a manual --pr 3237). The scraped URL donates only its NUMBER, and
// only when it names the same repo the remote decided; --pr overrides the
// number. divergent reports "the URL named some other repo" so the caller can
// log a note — the flag never changes the decision.
func reviewTarget(worktree, prURL string, pr int) (owner, repo string, number int, ok bool, divergent bool) {
	remotes := githubRemotes(worktree)
	if len(remotes) == 0 {
		return "", "", 0, false, false
	}
	owner, repo = remotes[0].owner, remotes[0].repo
	if prURL != "" {
		if uo, ur, un, good := review.ParsePRURL(prURL); good {
			if uo == owner && ur == repo {
				number = un
			} else {
				// Defense-in-depth: the URL named some other repo (possibly
				// another worktree remote). It must not donate its number to
				// the primary repo — that is the chimera shape again, one
				// remote over.
				divergent = true
			}
		}
	}
	if pr > 0 {
		number = pr
	}
	return owner, repo, number, number > 0, divergent
}

// acceptPRURL reports whether a scraped PR URL may be stored on state.PRURL
// (ADR-0023): its owner/repo must match ONE of the worktree's GitHub remotes.
// Local-only — `git remote`, no network, no gh — consistent with ADR-0006.
func acceptPRURL(raw, worktree string) bool {
	uo, ur, _, ok := review.ParsePRURL(raw)
	if !ok {
		return false
	}
	for _, rm := range githubRemotes(worktree) {
		if rm.owner == uo && rm.repo == ur {
			return true
		}
	}
	return false
}

// githubRemotes lists the worktree's GitHub remotes as owner/repo pairs —
// origin first, then any other remote. A worktree may legitimately use a
// different remote name (upstream, a fork remote, a deploy name); the review
// must work for ANY repo the job happens to sit in (owner directive
// 2026-10-10), so a non-origin name is never a dead end.
func githubRemotes(worktree string) []ghRemote {
	if worktree == "" {
		return nil
	}
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", append([]string{"-C", worktree}, args...)...)
		cmd.Env = envWithoutGitVars() // GIT_DIR from a hook env redirects -C at the wrong repo
		return cmd.Output()
	}
	var remotes []ghRemote
	if out, err := run("remote", "get-url", "origin"); err == nil {
		if o, r, good := parseGitRemote(strings.TrimSpace(string(out))); good {
			remotes = append(remotes, ghRemote{o, r})
		}
	}
	out, err := run("remote", "-v")
	if err != nil {
		return remotes
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
			remotes = append(remotes, ghRemote{o, r})
		}
	}
	return remotes
}

// ghRemote is one parsed GitHub remote of the worktree.
type ghRemote struct{ owner, repo string }

// remoteRepo derives owner/repo from the worktree's git remotes — origin
// first, then any other remote.
func remoteRepo(worktree string) (owner, repo string, ok bool) {
	remotes := githubRemotes(worktree)
	if len(remotes) == 0 {
		return "", "", false
	}
	return remotes[0].owner, remotes[0].repo, true
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
