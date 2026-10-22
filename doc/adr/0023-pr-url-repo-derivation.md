# 0023 — PR URL is validated against the worktree's git remote

*Status: Proposed* · *Date: 2026-10-22* · *Related: ADR-0006 (PR URL as run metadata), ADR-0012 (review campaigns), ADR-0020 (report = completion)*

## Context

The daemon scrapes `https://github.com/<owner>/<repo>/pull/<N>` out of the
session transcript as it tails it (ADR-0006) and stores the **first** link on
`job.State.PRURL`. That URL then flows into two consumers:

1. **The `watch`/`status` payload** — surfaced as the `pr_url` field and the
   round-done output-tail suffix.
2. **`reviewTarget` (internal/supervisor/review_target.go)** — resolves the
   owner/repo/PR-number a review campaign operates on.

`reviewTarget` currently lets the scraped `pr_url` **win** over the git
remote:

```go
// review_target.go (current)
if prURL != "" {
    if o, rp, n, good := review.ParsePRURL(prURL); good {
        return o, rp, n, true   // scraped URL decides owner + repo
    }
}
```

### The live defect (addictedproxy-llm-seo2, 2026-10-21)

That job's transcript contained the link
`https://github.com/nuxt/nuxt/pull/36542` — a false positive (the
AddictedProxy agent referenced a nuxt PR, as documented on 2026-10-21 in the
pi-supervisor ADR-0006 troubleshooting row). The campaign was armed manually
with `--pr 3237`, the real AddictedProxy PR number. `reviewTarget` parsed
owner=`nuxt`, repo=`nuxt`, number=`36542` from the garbage URL, then the `--pr`
override rewrote **only the number** to 3237. The campaign armed on
`nuxt/nuxt#3237` — a chimera of a real PR number and a wrong repo — and
reviewed an unrelated, ancient PR for its whole round budget.

The root failure: **owner/repo came from the transcript scrape (untrusted),
not from the git remote the job is actually working in.**

## Decision

Flip the authority. The **git remote of the worktree** is the source of truth
for `owner`/`repo`; the transcript scrape may only contribute the PR **number**,
and only when the link's owner/repo matches the worktree's remote.

### Rules

1. **owner/repo ALWAYS come from `git remote` of the job's worktree**
   (`remoteRepo(worktree)`, origin first, then any other remote whose host
   contains `github` — preserves the ADR-0012 fork workflow where origin is the
   fork). A worktree with no GitHub remote makes the campaign not-`ok`.
2. **A scraped PR URL is ACCEPTED only when its owner/repo matches one of the
   worktree's GitHub remotes.** A non-matching URL is discarded (never stored on
   `state.PRURL`), and the scraper keeps looking — `""` still means "not linked
   yet" (or only non-matching links seen), never "no PR exists" (ADR-0006
   semantics preserved).
3. **The PR number** is `review --pr N` if given; otherwise the number parsed
   from the accepted (matching) scraped URL; otherwise not-`ok`.

### Validation site

One gate, not three. Both scrape mechanisms — the `Detector` (first URL seen,
`stall.go`) and the `PRURLScanner` (last URL found) — funnel through
`Supervisor.recordPR` (supervisor.go:2891). The repo-match check lives
**inside `recordPR`**: validate `raw` against the runner's worktree before
assigning `r.state.PRURL`. This is local-only (`git remote`, stdlib/exec) — no
network, no GitHub auth, no `gh`, consistent with ADR-0006.

`reviewTarget` is simplified accordingly: owner/repo from the remote; the URL
number is used only if the URL is accepted; `--pr` overrides the number. The
scraped URL's owner/repo is no longer trusted (a defense-in-depth assertion
logs a note if they diverge, but cannot change the decision).

### Fork / multi-remote note

"Matches one of the worktree's GitHub remotes" handles the owner directive of
2026-10-10 (the review must work for ANY repo the job sits in): origin = the
fork or upstream as it is, and additional remotes are accepted. A PR opened on
a repo that is not a remote of the worktree is genuinely un-linkable here —
the operator should add it as a remote (or pass `--pr` and let the remote
supply owner/repo).

## Consequences

* **Positive:** the chimera cannot occur — the real repo always wins; garbage
  transcript links are ignored rather than latched. A false-positive scrape no
  longer steers a review campaign at the wrong repo.
* **Positive:** single validation point (`recordPR`); no duplication across
  the two scanners.
* **Behavioral change:** a transcript that links the *wrong* repo's PR (e.g.
  an upstream PR on a fork-PR job) will now stay `""` for that URL until a
  matching one appears. This is the owner's requested behavior ("discard that
  PR URL and wait to get the right one").
* **Local-only** — no network dependency added; the scrape stays best-effort.
* **Docs touch:** AGENTS.md invariant 9 gains the repo-validation clause;
  the `doc/skill/pi-supervisor` troubleshooting row gains the chimera case.

## Test plan

* `recordPR` rejects a non-matching URL (no state change, no log) and accepts a
  matching one (state + log). Fixture: a throwaway git worktree with
  `git remote add origin https://github.com/owner/repo.git` (no network needed
  for `git remote get-url`).
* `reviewTarget` chimera fixture: `prURL = nuxt/nuxt#36542`, `pr = 3237`,
  worktree remote = `owner/project` → result `owner/project#3237`, not
  `nuxt/nuxt#3237`.
* Scanner/Detector: a non-matching link never latches `prURL`; a later
  matching link does.
