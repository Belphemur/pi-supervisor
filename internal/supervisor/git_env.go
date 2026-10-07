package supervisor

import "os"

// envWithoutGitVars is os.Environ() minus every GIT_* variable.
//
// git exports GIT_DIR (and friends) into hook environments, and this daemon's
// surface runs inside them: pre-push fires `review-recheck`, whose git probes
// must read the JOB's worktree. An inherited GIT_DIR silently redirects
// `git -C <worktree> ...` at the supervisor's OWN repo — `-C` does not
// override it — so every job scoped to the wrong remote. Strip GIT_* for
// every spawned git whose target is a path we chose.
func envWithoutGitVars() []string {
	env := os.Environ()
	out := env[:0]
	for _, kv := range env {
		if len(kv) >= 4 && kv[:4] == "GIT_" {
			continue
		}
		out = append(out, kv)
	}
	return out
}
