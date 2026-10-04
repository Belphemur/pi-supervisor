---
name: pi_supervisor_review
description: "Review-campaign contract for pi: route every GitHub action through the pi-supervisor shim."
version: 1.0.0
author: Antoine Aflalo (Belphemur), Hermes Agent
license: MIT
platforms: [linux]
metadata:
  hermes:
    tags: [CodeReview, Pi, Orchestrator, GitHub, Supervisor]
    related_skills: [pi-supervisor, answer-code-review, pi]
---

# pi-supervisor review contract

You are running inside a **supervised review round**. The supervisor owns the
outer loop — it polls GitHub, decides whether another round is needed, and
enforces the round budget. You own one round's work: triage the threads, apply
the fixes, and answer every thread you touch.

**Every GitHub action goes through the shim.** Never run `gh`, never run
`reply_review.py`, never call the GitHub API yourself. The shim is the only
path, and the daemon holds the credentials — you never see a token.

## The transport

```bash
_pi-supervisor-review <verb> --job <job> [--pr N] [flags]
```

`$PI_SUPERVISOR_SOCKET` is set for you. The verb set is **closed** — there is
no other way to reach GitHub from this round:

| Verb | Use it to |
|---|---|
| `list_threads` | see every open thread (add `--pr N`) |
| `thread_detail` | read one thread's **full** body (`--thread-id PRRT_…`) |
| `post_replies` | answer N threads in one call (JSON array) |
| `resolve_thread` | close ONE thread you already answered this round |
| `bulk_resolve` | close many at once, ack-gated (rarely correct) |
| `check_ci` | read the CI verdict for the current head sha |

## Thread ids are `PRRT_…`, never numbers

Every `thread_id` you pass is a GraphQL review-thread node id, shaped
`PRRT_kwDOUDrzps6cpWH0`. They come from `list_threads` / `thread_detail`.

A bare number like `3904873498` is a **comment** node id from a different API,
and both mutations reject it with *"Could not resolve to a node with the global
id"*. If you find yourself holding a numeric id, you read the wrong field —
re-run `list_threads` rather than trying to adapt it. The daemon validates this
for you and answers `unknown-thread`, so this failure is cheap, but it wastes
a round.

## Call order: reply, THEN resolve

This is the one rule that fails if you get it backwards.

```bash
# 1. Answer every thread you triaged, in ONE batched call
_pi-supervisor-review post_replies --job <job> --pr <N> '[
  {"thread_id":"PRRT_a","type":"acceptance","body":"Fixed in abc1234 — ..."},
  {"thread_id":"PRRT_b","type":"rebuttal","body":"Not a regression: HEAD already ..."}
]'

# 2. Only then close them
_pi-supervisor-review resolve_thread --job <job> --thread-id PRRT_a
```

`resolve_thread` is refused with `not-answered-this-round` unless a
`post_replies` **in this same round** already touched that thread. Your instinct
will be to close a thread the moment you judge the work finished — here that is
the one thing that does not work.

The reply body is the record of *why* a finding was fixed, rebutted, or
deferred. A bare close throws that away, and the reviewer reads it as "the
finding was ignored".

**Scope the answer to the round.** A reply from round N-1 does not authorize a
close in round N. If you are resuming, re-read the open threads first.

## Reading a thread properly

`list_threads` gives a 120-char **snippet**, which for CodeRabbit is usually
just the title — the real recommendation hides behind a `<details>` block.
Always follow up with `thread_detail` before deciding:

```bash
_pi-supervisor-review thread_detail --job <job> --thread-id PRRT_kwDOUDrzps6cpWH0
```

## Triaging

For each open thread, pick one and say so in the reply's `type`:

- **`acceptance`** — it is a real, in-scope finding. Fix it, commit, push, and
  reference the commit sha in the body.
- **`rebuttal`** — it is wrong, or pre-existing, or out of scope. Reply with the
  evidence: cite the actual code at HEAD (`git show HEAD:<file>`), or state
  that it predates this PR. Do not change behaviour to satisfy a mistaken bot,
  and do not widen the PR's scope to "fix" unrelated code.

Two disciplines that are not optional:

- **Verify against the committed HEAD, not the thread's quoted snippet.** A
  reviewer's thread can describe an EARLIER push than your current HEAD. If HEAD
  already satisfies the finding, that is a `rebuttal` with evidence, not a fix.
- **Re-check after every push.** Reviewers re-review each push and open NEW
  threads. The open-thread count is per-commit, so a count taken before your
  final push is not authoritative.

## Pushing

```bash
git -C <worktree> add -A && git -C <worktree> commit -m "..." && git -C <worktree> push
```

Reference the sha in your replies. A round that answers threads but pushes
nothing is fine — the answer *is* the work — but a fix nobody pushed is not.

## Errors are symbols, not prose

Every failure is a non-zero exit plus a `reason` you can branch on:

| reason | exit | Do this |
|---|---|---|
| `not-answered-this-round` | 2 | call `post_replies` first |
| `unknown-thread` | 2 | the id is stale or wrong — re-run `list_threads` |
| `round-mismatch` | 2 | you are stale; re-read state |
| `no-live-round` | 2 | the round ended; stop, do not retry |
| `usage` | 2 | fix the call's arguments |
| `rate-limited` | 1 | back off, then retry |
| `auth-unavailable` | 1 | stop; this needs a human |
| `github-error` | 1 | retry with backoff |

Never parse the human-readable message to decide what to do. Branch on `reason`.

## bulk_resolve: almost never right

Closing many threads at once is legitimate only when you judge them
*collectively* out of scope or non-findings — for example a bot's whole sweep
about a file this PR never touched. Per-thread closure is the normal path.

```bash
_pi-supervisor-review bulk_resolve --job <job> --pr <N> \
  --reason "All 8 findings are about pre-existing logging in src/legacy/, untouched by this PR" \
  '["PRRT_a","PRRT_b"]'
```

This posts an audit comment on the PR and returns an `ack_id`. **It applies
nothing yet.** A peer must ack it (`pi-supervisor ack <job> --event <ack_id>`),
and an unacked request **expires** after 30 minutes, leaving the threads open.
So: post the reason, report the `ack_id`, and move on. Never claim you closed
them.

## Finishing the round

The daemon decides whether another round runs — not you. Do not merge, and do
not try to close the PR: the owner merges once the threads read zero. Your job
is to leave the threads answered, the fixes pushed, and CI reporting for the
current head sha.

Check where you stand at any time:

```bash
_pi-supervisor-review list_threads --job <job> --pr <N>
_pi-supervisor-review check_ci --job <job> --pr <N>
```