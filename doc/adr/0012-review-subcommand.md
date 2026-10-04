# ADR-0012: `pi-supervisor review` — daemon-orchestrated multi-round code review

## Status

Proposed. Awaiting joint design discussion on the open questions in §5.

## Context

Today every review job (`mealime-roomux-review`, `review184`,
`supervisor-review`, `ttfont-review`) runs `answer-code-review` as a **skill
passed to pi** — the LLM itself shells out to `gh`/`reply_review.py` to read
CodeRabbit threads, classify findings, reply inline, resolve, re-check after
each push, and gate on `pre-merge --pr N` before signalling done via its
marker. The orchestration logic (count → fix → re-count → repeat) lives in the
agent's head turn to turn, with no bounded budget the operator can set.

The request replaces that with a **daemon-owned review loop**: the supervisor
drives the outer round loop (poll → decide → trigger pi → poll), while pi
executes one round's fixes. Crucially, per the owner, **pi-supervisor fully
replaces `reply_review.py`: rather than the agent shelling out to ad-hoc
Python, an injected skill (`pi_supervisor_review`) carries the contract and
a CLI shim (`_pi-supervisor-review`) that exposes CLI-style commands; pi's
system prompt tells it to invoke those commands for every read/reply/resolve
action. The daemon is therefore never the mutation surface — it only *serves*
the reads and *records* the writes the shim asks it to do.

This reuses the control-flow shape ADR-0004 (`ci_stall`) and ADR-0011
(completion detection) already established for the daemon: poll an artifact,
decide based on what is seen, intervene.

## Decision

### 1. Two control planes, cleanly split

| Concern | Owner | Mechanism |
|---|---|---|
| **Outer loop** (poll GH → decide "one more pi round?" → enforce budget) | the daemon | `gh api` (read-only) + the round loop |
| **Fix execution + triage** (read a thread, classify fix/explain/defer, reply, resolve, push) | pi, via the `pi_supervisor_review` skill + `_pi-supervisor-review` shim | shim → daemon control socket |
| **Mutation surface** (post reply, resolve thread, push) | the daemon, as a server | `gh api` writes — but only when asked by the shim; the daemon never decides to |

The LLM does NOT shell out to `gh` directly anymore. For a review round the
job brief includes the `pi_supervisor_review` skill, whose system prompt
tells pi to route every GitHub review action through the
`_pi-supervisor-review` shim at the daemon's control socket (path in
`$PI_SUPERVISOR_SOCKET`); the shim is the only thing that ever emits a
review request. The daemon is MCP-**less** here — the shim speaks its JSON
control protocol directly.

### 2. Review API on the control socket

Per the owner: **no plugin/MCP server**. The surface pi uses is the daemon's
control socket directly, and the contract is carried as an **injected skill**
(`pi_supervisor_review`, shipped under `doc/skill/pi_supervisor_review/`)
that the system prompt loads for review rounds. The skill is two things in one:

- a plain-language contract — *"route every GitHub review action to the
  pi-supervisor at `$PI_SUPERVISOR_SOCKET` via `_pi-supervisor-review`; never
  call `gh` or `reply_review.py` from the shell"* — and
- a CLI shim (`_pi-supervisor-review list_threads …`, `_pi-supervisor-review
  post_reply …`, `… resolve_thread …`, `… check_ci …`) that talks to the
  daemon's control socket. The shim is what the system prompt's code blocks
  point at; the shim is the only thing that ever emits the request. The shim
  is **not** an MCP server — it speaks the daemon's JSON control protocol
  directly over the unix socket.

New control-socket method, additive only:

```
POST /review/action
{"action":"list_threads","pr":43,"state":"open","cursor":null}
  -> {"threads":[...],"cursor":"...","head_sha":"89a1a0c","ci":"green"}

POST /review/action
{"action":"post_reply","thread_id":3904873498,"body":"...","round":3}
  -> {"ok":true,"thread_id":3904873498,"head_sha":"NEW","ci_changed":false}

POST /review/action
{"action":"resolve_thread","thread_id":3904873498}
  -> {"ok":true}
```

The daemon authenticates the *call* by the live `review <job>` round it is bound
to (a control-socket request outside a running review round is refused) —
there is no user-facing auth on the shim because the shim runs *in* pi, the
daemon already knows which job owns this round, and the socket is `0600`.
Mutations are `gh api` writes; reads are `gh api` reads. All reuse the single
`gh` token from `~/.config/gh/hosts.yml` (confirmed: `repo` + `workflow`
scopes).

The shim is MCP-**less**: it speaks the daemon's JSON control protocol over
the unix socket and the daemon does the GH calls. The daemon's GH auth is an
internal detail that never reaches pi or the system prompt.

### 6. Auth posture

The daemon authenticates to GitHub in one of two ways, chosen per `Start`,
in priority order:

1. **GitHub App (preferred).** `GITHUB_APP_ID` + `GITHUB_APP_PRIVATE_KEY`
   (a path or inline PEM) → the daemon signs a JWT with
   `golang-jwt/jwt/v5` (`github.com/golang-jwt/jwt/v5`, the maintained
   v4+ successor) and exchanges it for an installation token via
   `go-github`'s `Apps.GetInstallationToken`. The token (1h TTL) is cached
   and refreshed by `go-github`'s `InstallationTokenSource`, so there is no
   hand-rolled refresh loop. The App is installed on the reviewed repos.
2. **`gh` token fallback.** If the App env is absent, `Start` runs
   `gh auth token` to obtain the active token (the *only* `gh` call in the
   daemon's lifecycle — used purely for token acquisition); that token is
   then passed to `go-github`/`githubv4` as a static token source. If
   `gh auth token` fails, `Start` refuses (exit 1) with `GitHub auth
   unavailable: run 'gh auth login' or set GITHUB_APP_ID`.

Two SDK deps, both current as of 2026:
`github.com/google/go-github/v90` (REST) + `github.com/shurcooL/githubv4`
(GraphQL). `go-github`'s `InstallationTokenSource` covers App-token refresh;
`go-githubv4`'s client wraps a plain `http.Client`, so both share one
authenticated transport and therefore one token source — the daemon never
holds two auth code paths to keep in sync.

The mutation API the daemon exposes over the control socket (`post_reply`,
`resolve_thread`, the CodeRabbit trigger comment) calls the GraphQL mutations
through `go-githubv4`, so the Q3 resolve-guard gets typed GraphQL errors
(`422 Resource not usable for resolve`, `43 Forbidden`) instead of parsed
strings.

This is an internal detail that never reaches pi or the system prompt.

The daemon does **not** re-implement the inline-answer-before-resolve rule from
the `answer-code-review` skill, nor its REST-reply-then-GraphQL-resolve path —
those live in the skill/`_pi-supervisor-review` shim, which is a thin adapter
over the GitHub SDKs. The daemon enforces the *budget* and the *loop*; it
does not triage or author replies.

### 3. Command surface

```
pi-supervisor review <job> --pr <PR> [--rounds N] [--skill <path>]
pi-supervisor review <job> --auto           # arm auto-trigger (§4)
```

- `--pr N` / `--auto` are mutually exclusive. `--pr` starts an explicit
  manual review campaign. `--auto` writes an `auto_review` stanza to the job
  def so the completion gate triggers one (§4).
- `--rounds N` — your "how many turns". Becomes this run's `MaxRounds`.
  **Default 2** (per the request), not the derived heuristic I floated earlier
  — an operator-set count beats a bot-computed one, and 2 matches the
  observed pattern (reply round + verification round).
- `--skill <path>` — the skill injected into pi's brief for the review
  rounds. **Defaults to `answer-code-review`** so the triage rules are
  unchanged; only the *orchestration* moves into the daemon. The brief also
  loads the `pi_supervisor_review` skill (the shim contract + system prompt),
  so a review round's prompt = `answer-code-review` triage rules *plus* the
  directive to route every GitHub action through `_pi-supervisor-review`
  instead of shelling out to `gh`.

`Start <job>` on a running review campaign refuses (`already running`); the
`--fresh` restart (ADR-0010) clears and resets the review loop.

### 4. Auto-trigger on PR + marker

When any job's round reaches its completion **marker** (ADR-0011) and that
round's transcript links a **PR** (`--pr`-style scrape, ADR-0006)
that is `OPEN`, and the job's def carries an `auto_review` stanza, the
supervisor:

1. Posts a `@codereplay please review` / `@coderabbitai review` trigger
   comment on the PR (so CodeRabbit starts a fresh pass — the owner's note:
   "No threads is normal [on a fresh PR; you] need to trigger CodeRabbit
   manually with a comment and then wait 5 minutes before checking for
   threads").
2. Waits 5 minutes (`review.coderabbit_warmup`, configurable; default
   `5m`).
3. After warmup: resolves the open-thread count via `gh`.
   - If > 0: arms a `review <job> --pr <N>` campaign with the stanza's
     `rounds` (default 2) and `skill`, emitting `review_armed`.
   - If still 0: emits `review_skipped` with `reason:"no open threads
     after CodeRabbit warmup"` — does not arm.

This makes `mealime-roomux` (marker `ALL_MEALIME_ROOMUX_DONE` + PR #43) →
review auto-run with 2 rounds, as specified. The trigger is structural: it
rides the sticky marker-latch from ADR-0011, so it fires the moment pi emits
the marker mid-round, not only at round-end classification.

The `pr_url` the marker-gate already scrapes (ADR-0006) is what the
auto-trigger reads — no second scrape. The PR number is derived by parsing the
linked `pull/<N>` out of that URL.

### 5. Open questions (decide before code)

Q1. **CodeRabbit trigger comment.** Hardcoded to `@coderabbitai review`?
Configurable via `review.trigger_comment` (default `@coderabbitai review`)
so a different bot / org convention is a one-line change, not a rebuild.

Q2. `--rounds 0` semantics. `0` = auto-derive from the thread count at arm
time (`ceil(open / 12)`, capped) as a convenience for large PRs. The default
remains 2. Worth keeping, or should 0 be an error to force an explicit count?

Q3. **Who enforces "answer every thread before resolve"?** Today the skill
does it by convention. With the daemon owning the loop, do you want the
daemon to *refuse* a `resolve_thread` request via the shim whose thread has no
agent-authored reply in the last N minutes (a cheap server-side guard), or
keep it as a skill/prompt rule only?

Q4. **Push detection.** If a round replied but did not push, the head SHA is
unchanged and the next round re-handles stale threads. Is that the policy you
want, or should a "no push" round count against the `MaxRounds` budget?

Q5. **Pre-merge gate.** This only gates *on* `pre-merge --pr` (ADR-pre-merge),
never executes it — confirming that matches your mental model.

## Consequences

- Review campaigns get the same bounded-budget discipline as every other job:
  `MaxRounds` is set at `Start`, visible in `status`, enforced by the loop.
  No free-running review.
- `watch <job>` becomes a real review dashboard: per-round open-thread count
  + head SHA + the remaining thread list, driven by the `round_done` event
  payload.
- No new daemon process model, no new credential surface — one read-only
  polling path (`gh api` reads) plus one mutation path (`gh api` writes, only
  on demand from the shim).
- `answer-code-review` skill is **retired for review jobs only**: replaced by
  `pi-supervisor review` + the `pi_supervisor_review` skill + the
  `_pi-supervisor-review` shim for the review shape. Non-review jobs are
  unchanged.
- The auto-trigger makes finishing a job and PR-ing it the natural handoff
  into review, with no operator command in between.
- **Auth prereq.** The preferred path requires a GitHub App installed on the
  reviewed repos (created once, `GITHUB_APP_ID` + `GITHUB_APP_PRIVATE_KEY`
  passed at `Start`). The `gh` fallback needs no setup, so local review jobs
  work with zero config once `gh auth login` is active.

## Non-goals

- Auto-merge (gated on, never executed, by `pre-merge --pr`).
- Non-GitHub review tools.
- Triaging findings in the daemon (counts only).
- Replacing the inline-answer-before-resolve rule or the
  REST-reply-then-GraphQL-resolve fallback — those stay in the
  skill/shim.
