# ADR-0019: Mid-round thread reminders — detect open review threads and steer the live session

Date: 2026-10-07
Status: Accepted

## Context

The campaign gate (ADR-0012 §4.2) checks threads BETWEEN rounds. Inside a
round the daemon is blind: pi may spend the whole turn on one thread (or
drift to unrelated work) while five others stay open, and the gate only
notices when the client exits. A round that ends with threads still open
costs a full round of budget to rediscover what the daemon already knew —
which threads were open and what their last comments say.

`steer` (ADR-0005) can deliver text to a LIVE round, and the review surface
already knows which threads are open (`ListThreads`) and which were answered
this round (`answeredInRound`, the ADR-0012 answer-before-resolve guard).
Neither is currently wired to the running session.

## Decision

**During a reviewing round, the daemon periodically re-lists open threads and
STEERS the live session with a digest of the ones still needing work.**

- A `watchThreadReminders` goroutine runs alongside the existing round
  watchers (`watchCIStalls`, `watchEmptyTurn`), only when the round runs in
  the `reviewing` phase with a captured transcript.
- Cadence: first check after `review_reminder_delay` (default 2 min) into the
  round, then a re-check ticker; a reminder repeats every
  `review_reminder_repeat` (default 10 min) while the open set is unchanged.
  Both are package vars so tests and operators can tune them; no new job-def
  surface.
- Content: every open (unresolved) thread NOT answered in the current round,
  one line each — thread id, last author, and the last comment's body
  (truncated). This is exactly the input the shim's reply/resolve verbs
  consume, so the model can act without a round-trip.
- Delivery: a PLAIN steer (`noWait`, never `--interrupt`). Reminders are
  advisory; interrupting the running turn to deliver one is an operator
  decision (ADR-0007), not the daemon's. The steer report is journaled with
  its outcome; a `no live round` outcome is normal for the moment the round
  ends and is not an error.
- An event `review_reminder` (non-terminal) records each delivered reminder —
  thread count and steer outcome — so `watch` shows why the session was
  nudged.
- Filter for truthfulness: threads answered in-round are excluded even if
  still unresolved on GitHub — the answer-before-resolve guard already
  authorizes their close; reminding about them would push the model to
  double-answer.

### Rejected

- Killing/restarting the round on open threads: burns round budget and a
  restart would quarantine the campaign's own session (ADR-0018). The gate
  between rounds already decides "one more round"; mid-round the fix is more
  information, not a new round.
- Steering the FULL thread detail: the digest is built from `ListThreads`
  (last comment per thread); `thread_detail` exists for the model that needs
  history. The reminder must stay one steer, not a paste of the PR.
- A cron/poll outside the round: violates the push-only delivery rule
  (invariant 6). The reminder lives and dies with the round it serves.

## Consequences

- A round that wanders gets pulled back to the open threads within minutes,
  instead of after the client exits.
- GitHub read traffic: one GraphQL list per ticker tick during reviewing
  rounds only (default every ~2–10 min), bounded by round duration — same
  budget class as the between-rounds gate.
- Reminder steers share the ctrl-file contract with operator steers (one
  frame, one ack). They never signal pi's process group.
- Round 1 of a fresh campaign session has no transcript yet
  (`sess == ""`), so reminders start on round 2+ — the first round is the
  brief read, and the gate catches anything it left open.