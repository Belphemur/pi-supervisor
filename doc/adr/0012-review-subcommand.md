# ADR-0012: `pi-supervisor review` — daemon-orchestrated multi-round code review

## Status

**Implemented and verified against a live GitHub PR.** All design questions
resolved (§8); the daemon, the shim, and the injected skill are on `master`.

Blocking external prerequisite for the *preferred* auth path only: the GitHub
App install (task `t_57751a5a`). The `gh auth token` fallback needs no setup and
is what the live tests exercise.

**Amended twice after the initial design — both times by evidence, not opinion:**

1. A review pass over this ADR, the `answer-code-review` skill and its
   `reply_review.py` helper, the daemon's marker gate, and the constraint that
   every command here is consumed by an LLM rather than a human. That
   restructured the document (each fact now has exactly one home) and corrected
   four design errors: thread identifiers, which API serves which read, the state
   transition the auto-trigger actually needs, and the human ACK that could never
   be granted.
2. Implementation plus a live test against a real PR, which found **four further
   defects in the design as written** — including two that would have made the
   feature unusable in production (the GraphQL input type names, and a
   required-ness source that silently returns nothing). All eight findings, with
   evidence and severity, are in **§9, Implementation findings log**.

No scope was added in either pass. Every correction was a correctness fix to the
accepted design.

## Context

Today every review job (`mealime-roomux-review`, `review184`,
`supervisor-review`, `ttfont-review`) runs `answer-code-review` as a **skill
passed to pi** — the LLM itself shells out to `gh`/`reply_review.py` to read
CodeRabbit threads, classify findings, reply inline, resolve, re-check after
each push, and gate on `pre-merge --pr N` before signalling done via its
marker. The orchestration logic (count → fix → re-count → repeat) lives in the
agent's head turn to turn, with no bounded budget the operator can set.

This replaces that with a **daemon-owned review loop**: the supervisor drives
the outer round loop (poll → decide → trigger pi → poll) while pi executes one
round's fixes. `pi-supervisor` fully replaces `reply_review.py`: instead of the
agent shelling out to ad-hoc Python, an injected skill (`pi_supervisor_review`)
carries the contract plus a CLI shim (`_pi-supervisor-review`), and pi's system
prompt routes every read/reply/resolve through it. The daemon serves the reads
and records the writes the shim asks for; it is never the deciding surface.

This reuses the control-flow shape ADR-0004 (`ci_stall`) and ADR-0011
(completion detection) already established for the daemon: poll an artifact,
decide based on what is seen, intervene.

**The consumer is an LLM.** Every verb in §2 is called by a model with no human
present: no prompts, no pagers, no TTY. That single constraint decides the ACK
design (§8, Q7), the `--skill` default (§8, Q2), and the error taxonomy (§2.4).

## Decision

### 1. Two control planes, cleanly split

| Concern | Owner | Mechanism |
|---|---|---|
| **Outer loop** (poll GH → decide "one more pi round?" → enforce budget) | the daemon | the round loop + one GitHub client |
| **Fix execution + triage** (read a thread, classify fix/explain/defer, reply, resolve, push) | pi, via `pi_supervisor_review` + `_pi-supervisor-review` | shim → daemon control socket |
| **Mutation surface** (post reply, resolve thread, post comment) | the daemon, as a server | GitHub GraphQL — only when the shim asks; the daemon never decides to |

pi does **not** shell out to `gh` for a review round. The MCP plugin server is
explicitly rejected: the shim speaks the daemon's JSON control protocol
directly over the unix socket, and the daemon's GitHub auth never reaches pi.

### 2. The shim contract (what the LLM calls)

The contract ships as an injected skill (`pi_supervisor_review`, under
`doc/skill/pi_supervisor_review/`) that the system prompt loads for review
rounds. It is two things: a plain-language directive (*route every GitHub
review action to the pi-supervisor at `$PI_SUPERVISOR_SOCKET` via
`_pi-supervisor-review`; never call `gh` or `reply_review.py` from the
shell*), and the shim CLI the directive points at. The shim is the only thing
that ever emits a review request.

#### 2.1 The verb set is closed

Six verbs. This is the whole surface — there is no other way to reach GitHub
from a review round.

| Verb | Purpose | Answers first? |
|---|---|---|
| `list_threads` | open/all threads + counts + `head_sha` + CI verdict | — |
| `thread_detail` | one thread's full untruncated body + comments | — |
| `post_replies` | reply to N threads in one call | **yes** |
| `resolve_thread` | close one thread | requires a same-round reply |
| `bulk_resolve` | close many, with an audit comment | ack-gated (§2.3) |
| `check_ci` | CI verdict for the current head sha | — |

#### 2.2 Call order is load-bearing

An LLM's natural instinct is to close a thread once it judges the work
finished. Here that is the one thing that fails:

```
post_replies  →  resolve_thread        (per thread, same round, always in this order)
```

`resolve_thread` is refused with `not-answered-this-round` unless a
`post_replies` in **this** round already touched that thread. Reply bodies are
the record of *why* a finding was fixed, rebutted, or deferred; a bare close
throws that away. The daemon stamps the round itself (§8, Q6) — the client
cannot name its own round, so the guard cannot be self-certified.

`post_replies` takes a **batch** (`(thread_id, type, body)` tuples) so one
model turn answers every thread it triaged, each with its own body and its own
`type{acceptance|rebuttal}` classification. One turn, N threads — the batch is
the LLM-friendly shape.

#### 2.3 `bulk_resolve` is ack-gated and expires

Bulk close posts a PR comment (thread ids + reason), emits
`bulk_resolve_requested` with an `ack_id`, and applies the mutations **only**
after a peer ACK (`pi-supervisor ack --event <ack_id>`). It returns
`pending_ack` + `ack_id` immediately — it never blocks the round.

An unacked request **expires** after `review.ack_timeout` (default `30m`) and
applies nothing, emitting `bulk_resolve_expired`; the threads stay open, which
is the safe default. Fail-closed, never a deadlock (§8, Q7).

#### 2.4 Errors are a closed set the caller can branch on

Every refusal is a stable symbolic `reason` plus a non-zero exit. No prose
parsing, no matching on message text.

| `reason` | Exit | Caller action |
|---|---|---|
| `no-live-round` | 2 | the campaign is not running; do not retry |
| `round-mismatch` | 2 | stale shim; re-read `list_threads` |
| `not-answered-this-round` | 2 | call `post_replies` first |
| `unknown-thread` | 2 | re-read `list_threads`; the id is stale or wrong |
| `auth-unavailable` | 1 | operator action; do not retry |
| `rate-limited` | 1 | retry with backoff |
| `github-error` | 1 | retry; the message carries the typed GH error |

`--json` is available on every verb for structured consumption. `review`,
`ack`, and `status` accept it at the CLI too (ADR-0008's exit contract — 2 =
usage/validation, 1 = refused, 0 = success — applies unchanged).

### 3. Wire protocol (daemon side)

One control-socket method, additive:

```
POST /review/action
{"action":"list_threads","pr":43,"state":"open","cursor":null}
  -> {"threads":[{"thread_id":"PRRT_kwDO...","resolved":false,
                  "last_author":"coderabbitai[bot]","snippet":"...",
                  "body":"<CodeRabbit markup stripped>"}],
      "cursor":null,"total":67,"head_sha":"89a1a0c","ci":"pass"}

POST /review/action
{"action":"thread_detail","thread_id":"PRRT_kwDO..."}
  -> {"thread_id":"PRRT_kwDO...","resolved":false,"body":"...",
      "comments":[{"author":"coderabbitai[bot]","body":"..."}]}

POST /review/action
{"action":"post_replies","pr":43,
 "replies":[{"thread_id":"PRRT_kwDO...","type":"acceptance","body":"..."},
            {"thread_id":"PRRT_kwDO...","type":"rebuttal","body":"..."}]}
  -> {"ok":true,"applied":["PRRT_kwDO...","PRRT_kwDO..."],
      "head_sha":"NEW","ci_changed":false}

POST /review/action
{"action":"resolve_thread","thread_id":"PRRT_kwDO..."}
  -> {"ok":true}          # or {"ok":false,"reason":"not-answered-this-round"}

POST /review/action
{"action":"bulk_resolve","pr":43,"thread_ids":["PRRT_kwDO..."],
 "reason":"<audit comment text>"}
  -> {"ok":true,"pending_ack":true,"ack_id":"brq-3-7f2a","threads":[...]}

POST /review/action
{"action":"check_ci","pr":43}
  -> {"verdict":"pass","checks":[{"name":"build","conclusion":"success"}]}
```

`job` and `round` are **not** client-supplied; the daemon takes both from its
live runner. A request that names a mismatched round is refused
(`round-mismatch`), not silently corrected.

#### 3.1 Thread ids are `PRRT_…` GraphQL node ids, end to end

The only `thread_id` this protocol accepts or emits is a GraphQL review-thread
node id (`PRRT_kwDO…`). The numeric `id` field from
`GET /pulls/<n>/comments` is a **comment** node id, and both mutations reject
it with *"Could not resolve to a node with the global id"* — the single most
common way to waste a review turn, and one an LLM will otherwise walk into by
grabbing the first `id` field it sees. The daemon is the sole producer of
thread ids here, so the shim never has to construct one.

#### 3.2 API assignment: GraphQL for threads, REST for PR/CI metadata

| Data | API | Why |
|---|---|---|
| thread list/detail (`isResolved`, bodies, authors) | **GraphQL** `reviewThreads` | the REST pulls-comments endpoint carries no thread identity, no resolution state, and no comment author |
| thread replies + resolves | **GraphQL** mutations | typed errors for the §2.2 guard |
| `head_sha`, PR open/closed, Actions job conclusions | **REST** (`go-github`) | plain metadata, no GraphQL needed |

Three query-shape constraints — two inherited from the proven queries, one found
by a live test. All three fail *silently* rather than loudly: an LLM reads a
schema error as "no threads yet" and polls forever.

- **`author` lives on the COMMENT node, not on the review thread.** Selecting
  `reviewThreads.nodes.author` fails schema validation on every poll.
- **Pagination runs to exhaustion.** `reviewThreads` returns oldest-first, so
  page 1 is mostly already-resolved history — a truncated read reports "0 open"
  while dozens are open (hit on PR #184, 67 threads). A `cursor:null` in the
  *response* means exhausted.
- **`thread_detail` needs an inline fragment on `node(id:)`** (live-test
  finding). `isResolved` lives on `PullRequestReviewThread`, not the `Node`
  interface, so selecting it off `node(id:)` fails with *"Field 'isResolved'
  doesn't exist on type 'Node'"*. Putting the fragment on the inner field does
  not satisfy the query builder either — it must wrap the whole selection.

The two mutations are asymmetric, which is its own silent failure:
`addPullRequestReviewThreadReply` takes **`pullRequestReviewThreadId`**, while
`resolveReviewThread` takes **`threadId`**. Same `PRRT_…` value, different field
name.

#### 3.3 CI verdicts: required checks only are blocking

`check_ci` reads the run's `/jobs` for the head sha observed at poll time — not
a `gh pr checks` rollup, which races state transitions right after a push and
transiently reads green with jobs still queued.

**Only checks the PR's protection rules REQUIRE block the campaign.** The
`/jobs` endpoint carries no required-ness flag, so the daemon asks GraphQL for
`checkRun.isRequired(pullRequestId:)` on the same head sha — the only
authoritative source. Inferring required-ness from a job name would be
guesswork, and guessing wrong in the permissive direction ships a red required
check.

| Situation | Verdict | Effect |
|---|---|---|
| required check `success`/`neutral`/`skipped` | `pass` | — |
| required check failing | `fail` | **blocks** |
| required check queued/in-progress | `pending` | **blocks** |
| optional check failing | `pass` | reported in `non_blocking`, ignored |
| optional check queued | `pass` | ignored |
| required-ness undeterminable | per-check, treating **all** as required | **blocks** |

Two properties matter here:

- **Optional failures are surfaced, not swallowed.** They appear in
  `non_blocking` and in the round message, so an operator sees them — they just
  cannot hold a campaign open. Without the split, one flaky non-required check
  would burn a 5-round budget and end `review_exhausted` with the real
  findings untouched.
- **Unknown required-ness fails closed.** If GitHub will not answer (missing
  permission, a non-PR ref), the rollup treats every check as required and sets
  `required_unknown`. A spurious "not done" costs rounds; a missed red check
  costs a broken merge. The strictness is visible rather than silent.

`neutral`/`skipped` count as passing (inherited from the `pre-merge` gate: they
are deliberate opt-outs, not failures).

> **Two of these rules are load-bearing and were found the hard way — see §9.2
> and §9.3.** Required-ness comes from branch protection, not `isRequired`, and
> an unprotected branch is a distinct outcome from an unknown one.

### 4. Campaign lifecycle

#### 4.1 `done → reviewing`, and why the trigger is not mid-round

When a round satisfies the **full** completion gate (ADR-0011: marker *and*
`final_report`) and the job def carries an `auto_review` stanza and the
transcript-linked PR (`pr_url`, ADR-0006) is `OPEN`, the job enters a review
phase:

1. The gate closes the build job: `done`, `done` emitted, round loop unwinds.
   `pr_url` is already on `state.PRURL`.
2. In the gate's tail: `auto_review` present + `PRURL` parses to an OPEN PR →
   the job transitions **`done → reviewing`**; `review_armed` is emitted.
3. Post `@coderabbitai review` so CodeRabbit starts a pass. A fresh PR
   legitimately has **zero** threads until it finishes — that is what the warmup
   is for, not a failure.
4. Wait `review.coderabbit_warmup` (default `5m`).
5. Re-check the open-thread count (GraphQL, paginated to exhaustion):
   - **> 0** → run the campaign with the stanza's `rounds` (default 5) + `skill`.
   - **0** → emit `review_skipped` with
     `reason:"no open threads after CodeRabbit warmup"`; leave the job `done`.

The transition is load-bearing (SOLID: one writer per job state). The gate
clears `active` and `Start` refuses a `done` job, so there is no live round for
a mid-round trigger to act from — an earlier draft claimed one and was not
implementable. The build job and the review phase therefore never both drive the
round loop: `done → reviewing` is a handoff, and review rounds **resume** the
same session under their own round counter and their own `MaxRounds`. The build
job's `done` verdict stands for the final report.

The trigger is **one-shot per `done` transition**: the marker latch is sticky
(ADR-0011), so an unguarded re-arm would loop forever. `auto_review` is
consumed when it fires; re-arming is an explicit `review <job> --auto`.

#### 4.2 Exit condition and round economics

The daemon evaluates the campaign's exit condition between rounds:
`0 open threads && CI pass` (§3.3) closes it as `review_done`. That is the
whole of the old `pre-merge` gate — steps 1 and 3 inherited, its wall-clock
`wait` replaced by the campaign's own `MaxRounds`. The daemon reports readiness
and **never merges** (the owner does).

Both conditions must hold: threads open with green CI is not done, and zero
threads with red required CI is not done. When it is not done, the round message
names the ONE blocking condition, because "not done" without a reason is what
makes an LLM re-arm blindly.

While a campaign is live it **owns the round budget** — the loop reads the
campaign's `MaxRounds`, not the build job's (a different, much larger number).
Otherwise a 5-round campaign would run to the job's default 200.

**No-push rounds are free.** Only pi-execution rounds consume `MaxRounds`. A
round that replied to threads but pushed no commit leaves `head_sha` unchanged,
so the next round re-handles the stale threads without burning budget —
including a rebuttal round whose evidence *is* the answer.

A campaign that exhausts its budget is a **distinct failure** from a build job
that never reached its marker: it emits `review_exhausted` with a message that
says threads are still open and how to re-arm with more budget. Reusing
"round cap reached without marker" would be a lie — there is no marker in a
review round.

### 5. Command surface

```
pi-supervisor review <job> --pr <PR> [--rounds N] [--type T] [--json]
pi-supervisor review <job> --auto [--json]
pi-supervisor ack --event <ack_id> [--json]
```

- `--pr N` / `--auto` are mutually exclusive. `--pr` starts a manual campaign;
  `--auto` writes the `auto_review` stanza that arms one (§4.1).
- `--rounds N` — becomes this run's `MaxRounds`. **Default 5** (review rounds
  are hours-long). `--rounds 0` auto-derives at arm time from the open-thread
  count: `ceil(open / review.per_round)` (`per_round` default 12), capped by
  `review.max_rounds` (default 12).
- `--type acceptance|rebuttal` — the campaign's round character, uniform across
  its rounds and recorded per round in `round_done`, so `watch`/`status` surface
  `#acceptance` vs `#rebuttal`. Mixed campaigns are two campaigns, not a flag
  that means different things per round.
- `Start <job>` on a running campaign refuses (`already running`);
  `restart --fresh` (ADR-0010) clears and resets the loop.

### 6. Auth posture

Chosen per `Start`, in priority order:

1. **GitHub App (preferred).** `GITHUB_APP_ID` + `GITHUB_APP_PRIVATE_KEY` (path
   or inline PEM) → the daemon signs a JWT with `golang-jwt/jwt/v5` and trades
   it for a 1h installation token via `go-github`'s
   `InstallationTokenSource`, which caches and auto-refreshes. No hand-rolled
   refresh loop, no PAT on disk.
2. **`gh` token fallback.** If the App env is absent, `Start` runs
   `gh auth token` — the *only* `gh` call in the daemon's lifecycle, used purely
   for token acquisition — and feeds it to both clients as a static source. On
   failure `Start` refuses (exit 1): `GitHub auth unavailable: run 'gh auth
   login' or set GITHUB_APP_ID`.

Deps, current as of 2026: `github.com/google/go-github/v90` (REST) +
`github.com/shurcooL/githubv4` (GraphQL) + `github.com/bradleyfalzon/ghinstallation/v2`
(App JWT + installation-token refresh). `githubv4` wraps a plain
`http.Client`, so both API clients share one authenticated transport and
therefore one token source — two SDKs, one auth path to keep in sync (DRY).

**Live-test corrections.** A read-only live test against a real PR
(`PI_SUPERVISOR_LIVE_GH=1`) overturned two assumptions:

1. **`checkRun.isRequired(pullRequestId:)` is not usable here.** The query
   returned *empty* check runs for a fork PR, so required-ness silently
   degraded to fail-closed — meaning every check blocked and any failing optional
   check would have burned the whole budget anyway. Required-ness now comes from
   the **base branch's protection rules** (`GET /branches/{branch}/protection`),
   which is the only place it exists; neither check-runs REST nor Actions /jobs
   carries a required flag.
2. **`no_protection` is a distinct outcome from `required_unknown`.** A branch
   with no protection requires *nothing*, so nothing blocks. Conflating that with
   "unknown → assume all required" deadlocks every campaign on an unprotected
   repo. Note go-github reports the 404 as a plain
   `errors.New("branch is not protected")`, **not** an `*ErrorResponse` — so
   detection matches on the message as well as the status.

> **Correction, found at implementation time.** This section originally named
> `go-github`'s `InstallationTokenSource` as the App-token refresher.
> **go-github v90 removed `AppsTransport` and `InstallationTokenSource`** — its
> own package docs now point at `ghinstallation/v2`, so that is what the daemon
> uses (`NewAppsTransport` for the App JWT, then `NewFromAppsTransport` for the
> installation-scoped, auto-renewing token). v90 additionally renamed `Pulls` →
> `PullRequests` and `Apps.FindRepositoryInstallation` →
> `GetRepositoryInstallation`. The intent is unchanged — no hand-rolled JWT or
> refresh loop — but the named helpers no longer exist.

The daemon enforces the *budget* and the *loop*; pi triages and authors replies.
What carries over from `answer-code-review` is its **triage vocabulary** (fix /
explain-non-issue / defer-out-of-scope), which the `post_replies` `type` field
names, and its proven query shapes (§3.1, §3.2) — inherited, not re-derived.

### 7. `watch` as the review dashboard

Each `review_round_done` carries round type, open-thread count, CI verdict (with
the failing **required** checks named), and the round budget, so a campaign's
shape is legible mid-flight.

**Terminality is part of the contract.** `watch` closes its stream on a terminal
event, and `watch -t` exits on one, so getting this wrong either truncates a live
campaign or hangs a finished one:

| Event | Terminal? | Why |
|---|---|---|
| `reviewing` | **no** | the build job is done but the campaign still owns the loop |
| `review_round_done` | no | progress, not an outcome |
| `review_armed`, `review_auto_armed`, `review_skipped` | no | setup/skip, the job still runs |
| `bulk_resolve_requested` / `_applied` / `_expired` | no | a sub-event of a running round |
| `review_done` | **yes** | the campaign closed clean |
| `review_exhausted` | **yes** | the campaign closed on budget |

`reviewing` is the load-bearing one. The completion gate closes the *build* job
and enters the review phase in the same step, so emitting `done` there — which
is terminal — would print "THE RUN IS OVER", exit 0, and stop listening while the
campaign was still running. The gate therefore emits `reviewing`, never `done`,
on the handoff path.

Every review event gets an LLM-facing footer that says what to do next, and the
messages avoid the traps that cost rounds: `reviewing` explicitly says the job
is NOT over; `bulk_resolve_requested` says nothing was closed yet and names the
ack command; `bulk_resolve_expired` says the threads are still open;
`review_exhausted` gives the re-arm command with more budget.

### 8. Decision log

Each entry names the trade, per `coding-philosophy` — which principle won and
what the loser would have cost.

- **Q1 — Trigger comment hardcoded.** The auto-trigger posts the fixed string
  `@coderabbitai review`. No `review.trigger_comment`. *KISS:* a config knob for
  a string that never varies in this deployment is surface without a user;
  changing bots is an ADR amendment.
- **Q2 — `--skill` defaults to `pi_supervisor_review`, not
  `answer-code-review`.** An earlier draft defaulted to `answer-code-review` for
  "unchanged triage rules" while §2 forbids the agent from touching
  `reply_review.py` — but that skill's body is a set of instructions to run
  exactly that script. Injecting both puts two contradictory directives in one
  prompt, and an LLM will follow whichever it reads last. *DRY:* the triage rules
  are extracted into `pi_supervisor_review`; there is one instruction source, not
  two that disagree. `answer-code-review` remains available via explicit
  `--skill` for prose reference, and stays the default for **non**-review jobs.
- **Q3 — Two SDKs, not one.** `go-githubv4` cannot do REST Actions queries and
  `go-github` cannot express `reviewThreads` as a typed review-thread model.
  *DRY over SOLID:* both wrap the same `http.Client`, so the token source stays
  single; a third "unified client" abstraction would duplicate knowledge to
  avoid one import line.
- **Q4 — Round defaults locked.** `--rounds` default 5; `--rounds 0` derives
  `ceil(open / review.per_round)`. *KISS:* the cap is the config key
  `review.max_rounds` — an earlier draft cited a `MAX_ROUNDS_DEFAULT` Go constant
  that does not exist, which sends an implementer hunting for unwritten code.
- **Q5 — Resolve guard is server-side.** `resolve_thread` is refused unless this
  round's `post_replies` touched that thread (§2.2). *SOLID:* the invariant must
  hold against a confused caller, so it lives in the one place that knows the
  round's bookkeeping rather than in the skill's prose.
- **Q6 — The daemon stamps `job` and `round`.** Never taken from the request
  payload; a mismatch is refused, not corrected. *SOLID:* if the client could
  name its own round the guard would be self-certifying — exactly the
  false-delivery class ADR-0005 exists to kill.
- **Q7 — Bulk close is ack-gated with an expiry, not human-gated.** An earlier
  `pending_human_ack` implied an operator who is never present, so it would have
  blocked forever. An LLM consumer cannot be asked to click. *SOLID (fail
  closed):* nothing closes without an ack, and an unacked request expires to
  "threads stay open" — the safe direction. *KISS:* no prompt, no modal, one
  `ack` verb.
- **Q8 — CI from the Actions jobs endpoint, head-sha scoped, and only
  REQUIRED checks block** (§3.3). *SOLID:* the `gh pr checks` rollup races state
  transitions, so a verdict can be green for a commit whose jobs have not run — a
  false pass on the only gate that guards a merge. Required-ness comes from
  `isRequired(pullRequestId:)`, not from a job-name guess, and unknown
  required-ness fails closed: a spurious "not done" costs rounds, a missed red
  required check costs a broken merge.
- **Q9 — The verb set is closed at six, with a stable `reason` enum** (§2.1,
  §2.4). *KISS:* an LLM cannot reliably branch on prose; a closed enum can be
  exhaustive-checked and unit-tested.
- **Q10 — No `--merge`.** The daemon reports readiness; the owner merges. *KISS
  and safety:* an unattended agent that can merge is an unattended agent that
  can merge the wrong SHA.

## 9. Implementation findings log

Every entry below was found **after** §1–§8 were accepted, by implementing the
code and then testing it against a real GitHub PR
(`Belphemur/XPoint#171`, 21 threads). Each is a defect in the design *as
written*, not an implementation slip — which is the argument for the live test
existing at all. Ordered by severity.

| # | Finding | Severity | Resolution |
|---|---|---|---|
| 1 | `post_replies` / `resolve_thread` / `bulk_resolve` **could never have worked** | **fatal** | input types renamed to GitHub's exact names |
| 2 | Required-ness source returns nothing | **fatal** | branch-protection API |
| 3 | Unprotected branch read as "unknown" | **high** | `no_protection` is now distinct |
| 4 | `thread_detail` query invalid | **high** | inline fragment required |
| 5 | `watch` truncated live campaigns | **high** (pre-implementation) | `reviewing` is non-terminal |
| 6 | Campaign budget ignored | **high** (pre-implementation) | campaign owns `MaxRounds` |
| 7 | No campaign exit condition | **high** (pre-implementation) | `reviewGate` |
| 8 | Every check blocked, optional included | **high** (pre-implementation) | required-only blocking |

### 9.1 The mutations were unusable — input type names are load-bearing

`shurcooL/graphql` derives the GraphQL **input type name from the Go type
name**. The structs were named for their role (`addReplyInput`,
`resolveInput`), so GitHub answered:

```
addReplyInput isn't a defined input type (on $input)
```

Every reply the daemon sent would have failed. The structs are now
`AddPullRequestReviewThreadReplyInput` and `ResolveReviewThreadInput`. No unit
fixture could catch this: a hand-rolled `http.Client` accepts anything.

**The lesson generalises:** with a generated GraphQL client, the *Go type name*
is wire contract. Renaming a struct is an API change.

### 9.2 `checkRun.isRequired` returns nothing for a fork PR

The ADR (§3.3, as first written) named `isRequired(pullRequestId:)` as the
authoritative source of required-ness. Live, it returns **empty check runs** for
a PR whose base is unprotected. The failure mode was silent and inverted: empty
→ "unknown" → *treat everything as required* → **every** optional check blocking,
which is precisely the livelock §8/Q8 exists to prevent.

Required-ness now comes from `GET /repos/{o}/{r}/branches/{base}/protection`,
which is the only place it exists — neither the check-runs REST endpoint nor
Actions `/jobs` carries a required flag.

### 9.3 Unprotected is the opposite of unknown

These two must not share a code path:

| State | Meaning | Effect |
|---|---|---|
| `no_protection` | the branch requires nothing | **nothing blocks** |
| `required_unknown` | protection exists, list unreadable | **everything blocks** (fail closed) |
| known | the list is authoritative | required checks block |

Conflating them deadlocks every campaign on an unprotected repo. A further trap:
go-github reports the 404 as a plain `errors.New("branch is not protected")`,
**not** an `*ErrorResponse`, so a status-code check alone never fires.

### 9.4 `node(id:)` needs an inline fragment

`isResolved` lives on `PullRequestReviewThread`, not on the `Node` interface, so
`node(id: $id) { isResolved }` fails with *"Field 'isResolved' doesn't exist on
type 'Node'"*. The fragment must wrap the **whole** selection; putting it on the
inner field is rejected too. Both misplacements were hit live.

### 9.5 What the live tests are, and what they deliberately are not

`internal/review/live_gh_test.go` (reads) and `live_gh_mutate_test.go` (writes)
are opt-in:

```bash
# read-only
PI_SUPERVISOR_LIVE_GH=1 PI_SUPERVISOR_LIVE_REPO=owner/name PI_SUPERVISOR_LIVE_PR=N \
  go test ./internal/review/ -run Live -v

# writes: posts replies and resolves threads
PI_SUPERVISOR_LIVE_GH=1 PI_SUPERVISOR_LIVE_MUTATE=1 … -run LiveMutate -v
```

The write path has a **separate** env gate on purpose: reading a PR is harmless,
posting to it is not, and nobody should mutate a PR because they left one variable
set. Every probe reply is tagged `[pi-supervisor live-probe]`, and every thread
the tests resolve is **re-opened afterwards**, so a run leaves the PR as found.

Verified live on #171: 21 threads all `PRRT_`-prefixed, CodeRabbit bodies cleaned,
`open → answered → resolved` confirmed by read-back, all three probe threads
re-opened. The GitHub-mutating verbs are the only paths in this ADR that cannot be
proven without a live PR — which is exactly why the write test exists.

## Consequences

- Review campaigns get the same bounded-budget discipline as every other job:
  `MaxRounds` set at `Start`, visible in `status`, enforced by the loop, and
  only pi-execution rounds consume it.
- Every review verb is LLM-callable: no prompts, no pagers, no TTY assumptions,
  a closed error enum, and `--json` throughout. This constraint is what removed
  the human ACK from the design.
- `answer-code-review` is retired **as the review-jobs default**: its triage
  rules live in `pi_supervisor_review`, its query shapes are inherited by the
  daemon, and its `gh`/`reply_review.py` plumbing is replaced by the shim.
  Non-review jobs are untouched.
- No new daemon process model and no new credential surface: two GitHub SDK
  clients sharing one token source, plus `gh auth token` for acquisition only.
- Finishing a job and PR-ing it becomes the natural handoff into review, with no
  operator command in between.
- **Auth prereq.** The preferred path needs a GitHub App installed on the
  reviewed repos (created once; `GITHUB_APP_ID` + `GITHUB_APP_PRIVATE_KEY` at
  `Start`). The `gh` fallback needs no setup.

## Non-goals

- Auto-merge (§8, Q10).
- Non-GitHub review tools.
- Triaging findings in the daemon. It counts threads and gates rounds; pi writes
  every reply body.
- An interactive approval step — there is no human at this terminal to press a
  key (§8, Q7).
- Re-deriving the review-thread query shapes and id rules. They are inherited
  from `answer-code-review`'s proven queries (§3.1, §3.2).