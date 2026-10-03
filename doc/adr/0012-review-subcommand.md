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
replaces `reply_review.py`**: rather than the agent shelling out to ad-hoc
Python, a pi plugin (`pi_supervisor_review`, an MCP tool) exposes CLI-style
commands, and pi's system prompt tells it to invoke those commands for every
read/reply/resolve action. The daemon is therefore never the mutation surface
— it only *serves* the reads and *records* the writes the plugin asks it to do.

This reuses the control-flow shape ADR-0004 (`ci_stall`) and ADR-0011
(completion detection) already established for the daemon: poll an artifact,
decide based on what is seen, intervene.

## Decision

### 1. Two control planes, cleanly split

| Concern | Owner | Mechanism |
|---|---|---|
| **Outer loop** (poll GH → decide "one more pi round?" → enforce budget) | the daemon | `gh api` (read-only) + the round loop |
| **Fix execution + triage** (read a thread, classify fix/explain/defer, reply, resolve, push) | pi, via the `pi_supervisor_review` plugin | plugin → daemon RPC (the daemon *serves* the read/write) |
| **Mutation surface** (post reply, resolve thread, push) | the daemon, as a server | `gh api` writes — but only when asked by the plugin; the daemon never decides to |

The LLM does NOT shell out to `gh` directly anymore. Its system prompt for a
review round is: *"use `pi_supervisor_review` for every GitHub review action:
list threads, get a thread, post a reply, resolve a thread, check CI. Never
call `gh` or `reply_review.py` from the shell."* The plugin is an MCP stdio
server that talks to the daemon's control socket.

### 2. The plugin-to-daemon RPC

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

The daemon authenticates the *call* by the job it is bound to (a socket
request without a live `review <job>` round is refused) — there is no
user-facing auth on the plugin because the plugin runs *in* pi, the daemon
already knows which job owns this round, and the socket is `0600`.
Mutations are `gh api` writes; reads are `gh api` reads. All reuse the single
`gh` token from `~/.config/gh/hosts.yml` (confirmed: `repo` + `workflow`
scopes).

The daemon does **not** re-implement the inline-answer-before-resolve rule from
the `answer-code-review` skill, nor its GraphQL-vs-REST reply path — those
live in the plugin, which is a thin adapter over `gh`. The daemon enforces
the *budget* and the *loop*; it does not triage or author replies.

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
- `--skill <path>` — the skill pi loads as its system-prompt source for the
  review rounds. **Defaults to `answer-code-review`** so the triage rules are
  unchanged; only the *orchestration* moves into the daemon.

`Start <job>` on a running review campaign refuses (`already running`); the
`--fresh` restart (ADR-0010) clears and resets the review loop.

### 4. Auto-trigger on PR + marker

When any job's round reaches its completion **marker** (ADR-0011) and that
and that round's transcript links a **PR** (`--pr`-style scrape, ADR-0006)
that is `OPEN`, and the job's def carries an `auto_review` stanza, the
supervisor:

1. Resolves the PR's current open-thread count via `gh` (the trigger guard).
2. If > 0 open threads: arms a `review <job> --pr <N>` campaign with the
   stanza's `rounds` (default 2) and `skill`, emitting a `review_armed` event.
3. If 0 threads are open: does **not** arm — a PR with nothing left to review
   does not auto-start a review loop; emits `review_skipped` with
   `reason:"no open threads"`.

This makes `mealime-roomux` (marker `ALL_MEALIME_ROOMUX_DONE` + PR #43) →
review auto-run with 2 rounds, as specified. The trigger is structural: it
rides the sticky marker-latch from ADR-0011, so it fires the moment pi emits
the marker mid-round, not only at round-end classification.

The `pr_url` the marker-gate already scrapes (ADR-0006) is what the
auto-trigger reads — no second scrape. The PR number is derived by parsing the
linked `pull/<N>` out of that URL.

### 5. Open questions (decide before code)

Q1. **Plugin transport.** MCP stdio server on the control socket is my default
because it's the daemon's existing story (`watch`/`steer` already use the
socket). A direct HTTP-to-plugin is simpler to implement but adds a second
listener. MCP-stdio preferred unless you want standalone `gh`-wrappers you can
also curl.

Q2. `--rounds 0` semantics. I propose: `0` = auto-derive from the thread count
at arm time (`ceil(open / 12)`, capped), as a convenience for large PRs. The
*dafult* stays 2. Acceptable, or should 0 be an error?

Q3. **Who enforces "answer every thread before resolve"?** Today the skill
does it by convention. With the daemon owning the loop, do you want the
daemon to *refuse* a `resolve_thread` RPC whose thread has no agent-authored
reply in the last N minutes (a cheap server-side guard), or keep it as a
skill/prompt rule only?

Q4. **Push detection.** If a round replied but did not push, the head SHA is
unchanged and the next round re-handles stale threads. Is that the policy you
want, or should a "no push" round be a strike toward the `MaxRounds` budget?

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
  polling path (`gh api` reads) plus one mutation path (`gh api` writes, only on
  plugin demand).
- `answer-code-review` skill is **retired for review jobs only**: replaced by
  `pi-supervisor review` + the `pi_supervisor_review` plugin for the review
  shape. Non-review jobs are unchanged.
- The auto-trigger makes finishing a job and PR'-ing it the natural handoff
  into review, with no operator command in between.

## Non-goals

- Auto-merge (gated on, never executed, by `pre-merge --pr`).
- Non-GitHub review tools.
- Triaging findings in the daemon (counts only).
- Replacing the inline-answer-before-resolve rule or the
  REST-reply-then-GraphQL-resolve fallback — those stay in the plugin/skill.
