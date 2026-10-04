# ADR-0012: `pi-supervisor review` — daemon-orchestrated multi-round code review

## Status

Accepted. All design questions resolved (§5); ready for implementation.
Blocking external prerequisite: the GitHub App install (task `t_57751a5a`).

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
| **Outer loop** (poll GH → decide "one more pi round?" → enforce budget) | the daemon | `go-github` reads + the round loop |
| **Fix execution + triage** (read a thread, classify fix/explain/defer, reply, resolve, push) | pi, via the `pi_supervisor_review` skill + `_pi-supervisor-review` shim | shim → daemon control socket |
| **Mutation surface** (post reply, resolve thread, push) | the daemon, as a server | `go-githubv4` mutations — but only when asked by the shim; the daemon never decides to |

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
Reads go through `go-github`; mutations through `go-githubv4` (§6).

The shim is MCP-**less**: it speaks the daemon's JSON control protocol over
the unix socket and the daemon does the GH calls. The daemon's GH auth is an
internal detail that never reaches pi or the system prompt.

### 3. Command surface

```
pi-supervisor review <job> --pr <PR> [--rounds N] [--skill <path>]
pi-supervisor review <job> --auto           # arm auto-trigger (§4)
```

- `--pr N` / `--auto` are mutually exclusive. `--pr` starts an explicit
  manual review campaign. `--auto` writes an `auto_review` stanza to the job
  def so the completion gate triggers one (§4).
- `--rounds N` — your "how many turns". Becomes this run's `MaxRounds`.
  **Default 5** (review rounds are hours-long). `--rounds 0` = auto-derive
  from the thread count at arm time (`ceil(open / 12)`, capped by
  `MAX_ROUNDS_DEFAULT`); the operator-set default wins for explicit calls.
- `--type acceptance|rebuttal` — declares the round's character. An
  `acceptance` round is handed threads treated as accepted findings to fix;
  a `rebuttal` round is where the agent pushes back with evidence. Recorded
  per round in `round_done` so stats surface `#acceptance` vs `#rebuttal`
  in `watch`/`status`.
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
3. After warmup: resolves the open-thread count via `go-github`.
   - If > 0: arms a `review <job> --pr <N>` campaign with the stanza's
     `rounds` (default 5 — review rounds are hours-long) and `skill`, emitting `review_armed`.
   - If still 0: emits `review_skipped` with `reason:"no open threads
     after CodeRabbit warmup"` — does not arm.

This makes `mealime-roomux` (marker `ALL_MEALIME_ROOMUX_DONE` + PR #43) →
review auto-run with 5 rounds, as specified. The trigger is structural: it
rides the sticky marker-latch from ADR-0011, so it fires the moment pi emits
the marker mid-round, not only at round-end classification.

The `pr_url` the marker-gate already scrapes (ADR-0006) is what the
auto-trigger reads — no second scrape. The PR number is derived by parsing the
linked `pull/<N>` out of that URL.

### 5. Resolved questions

**Q1 — CodeRabbit trigger comment: hardcoded.** The auto-trigger posts a
fixed `@coderabbitai review` comment. No `review.trigger_comment` config
field: one bot, one convention, and a config knob for a string that never
varies in this deployment is surface without a user. Changing bots means an
ADR amendment, not a config edit.

**Q2 — `--rounds 0`** = auto-derive (`ceil(open/12)`, capped). Default stays
5 on explicit calls.

**Q3 — Resolve guard: server-side (option A).** Both round types
(`acceptance` and `rebuttal`) answer-and-resolve, so `resolve_thread` refuses
with `409 already-resolved-without-reply` unless the daemon recorded a
`post_reply` on that `thread_id` from this job's authenticated user in the
current round. Typed `go-githubv4` errors make the guard inescapable.

**Q4 — Push detection: no-push is free.** Only pi-execution rounds consume
`MaxRounds`.

**Q5 — Pre-merge is a gate, never an action.** Full context:

The existing gate is `reply_review.py pre-merge --pr N`, which in the current
skill does four things in order:

1. *(optional)* `wait` until CI green **and** threads resolved, or timeout;
2. *(optional)* `close_all` — bulk-resolve every still-open thread, which
   *assumes* they were already answered;
3. evaluate the gate — fail if **any** thread is open **or** any required CI
   check is not passing (`neutral`/`skipped` count as passing);
4. *(optional)* squash-merge via `PUT /repos/{owner}/{repo}/pulls/{n}/merge`.

The review loop **inherits steps 1 and 3 only, and never passes
`--auto-close` or `--merge`**:

- **No `--auto-close` (step 2).** That flag is exactly the behavior ADR-0012
  §Q3 forbids: bulk-resolving threads the agent never answered. The loop's
  resolve guard makes bulk-close structurally impossible — each thread must
  carry its own agent reply in the round that closed it.
- **No `--merge` (step 4).** The owner merges. Same contract as the existing
  "final report + marker ⇒ done": the supervisor's job ends at "the PR is
  ready", not "the PR is merged".
- **Steps 1 and 3 become the loop's own exit condition.** The loop already
  polls threads and CI every round (§2 `list_threads` returns `ci`), so
  "0 open threads && CI pass" is the `done` classification, and the timeout
  arm is `MaxRounds` rather than a wall-clock `wait`. The separate
  `pre-merge --pr` invocation at the end of a campaign is therefore
  *redundant* — the daemon already holds both facts.

So the practical contract: **the daemon reports readiness; it never merges and
never bulk-closes.** A human runs `pre-merge --pr N` (or merges directly) if
they want the independent second opinion. If you would rather the loop keep
a final explicit `pre-merge --pr N` call as a belt-and-braces check before
classifying `done`, say so — it is cheap, but it re-introduces a `gh api`
call outside the SDK surface and duplicates state the daemon already has.

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

## Consequences

- Review campaigns get the same bounded-budget discipline as every other job:
  `MaxRounds` is set at `Start`, visible in `status`, enforced by the loop, and
  only pi-execution rounds consume it (a no-push answer round is free). No
  free-running review.
- `watch <job>` becomes a real review dashboard: per-round `round_done` lines
  (round type `#acceptance`/`#rebuttal`, open-thread count, head SHA, rc,
  pushed?) + the remaining thread list, driven by the event payload. Stats
  surface `#accepted` / `#overridden` (rebuttal) counts.
- No new daemon process model, no new credential surface — one GitHub SDK
  client (`go-github` + `go-githubv4`, shared token source) plus one
  `gh auth token` fallback for token acquisition only.
- Per-round `--type acceptance|rebuttal`; both answer-and-resolve, so the
  daemon enforces a server-side guard: `resolve_thread` returns `409`
  unless the daemon recorded a `post_reply` on that thread from this job's
  `gh` user in the current round — inescapable, typed via `go-githubv4`.
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
