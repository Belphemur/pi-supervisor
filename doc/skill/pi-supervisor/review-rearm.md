---
name: supervisor-review-rearm
description: "Use when adding or changing pi-supervisor's ADR-0012 review loop: campaign arming, the answer-before-resolve guard, review verbs, or post-completion thread detection. Explains why the campaign is one-shot and why thread re-checks are event-driven, not polled."
version: 1.0.0
author: Antoine Aflalo (Belphemur), Hermes Agent
license: MIT
metadata:
  hermes:
    tags: [pi-supervisor, review, adr-0012, supervision, github]
    related_skills: [pi-supervisor, pi-orchestrator, answer-code-review]
---

# The review loop is one-shot, and that is deliberate

ADR-0012's review campaign answers and resolves PR threads from inside a pi
round. Two properties are load-bearing and easy to break by accident.

## 1. The campaign is consumed once per arming

`autoReviewHandoff` clears `r.autoReview` **unconditionally** — whether or not
the PR turned out to be usable. The marker latch is sticky (ADR-0011), so
leaving the stanza armed would re-arm on every later round and loop forever.
Same reason `bulk_resolve` acks expire (ADR-0012 §2.3): fail closed, never
deadlock, never silent.

Consequence: **findings that appear after the campaign ends are not the
campaign's problem.** Bots re-review the whole diff after *every* push, so a
campaign that closes clean can be followed minutes later by new findings. That
is a new unit of work, and the answer is a new arming
(`pi-supervisor review <job> --pr N`), not a resurrected campaign.

## 2. Never poll GitHub for thread counts

The tempting design is a ticker that re-lists threads and re-arms. Do not. It
burns API quota forever, keeps the token warm for no reason, and violates the
push-only delivery rule (AGENTS.md invariant 6): `watch` streams from the
in-process broker, and cronjob/polling delivery paths are explicitly non-goals.

The signal that matters is **free and already local**: a push moves the git HEAD
the daemon can see. So the check is edge-triggered off a real event.

See `references/thread-recheck.md` for the implemented design: a recorded
baseline (`threads_at_close`) compared against a fresh count on the next push
or explicit `review recheck`, emitting `review_threads_appeared`.

## Pitfalls

- **Do not treat "0 open threads" as "no review".** CodeRabbit skips repos
  under 10 stars and posts a *manual trigger* comment instead; Copilot and
  kody-ai have their own cadences. Zero threads can mean a quota window, a
  manual gate, or a genuinely clean PR — read the bot's comment before
  concluding.
- **Every review verb requires a live round** (`no-live-round`, exit 2). The
  shim is valid only inside its own round; that gate is what stops an
  out-of-round caller from learning anything about thread validity or PR
  existence (ADR-0012 §4).
- **The daemon stamps `job`/`round`**; a client-supplied round is checked, never
  used, and a mismatch is refused (`round-mismatch`).
- **Do not resolve a thread without an inline reply.** Use
  `answer-code-review`'s `answer`/`bulk --resolve`; kody-ai and Qodo do not
  auto-resolve.
