# ADR-0026: Round ends on agent_settled — retry-boundary agent_end is not the round's verdict

## Status

Accepted (2026-11-25). Root-caused from the xerif-sync-resolve round-cap
fatal (2026-11-25 17:09–17:11 EST): six rounds, six identical
`429: org_token_rate_limit_day` responses, zero model switches. The fix is a
client-side round-end semantics change; the pi-model-fallback extension and
the explabs provider plugin are correct and stay untouched.

## Context

The daemon classifies a round by pi's exit frames (ADR-0002): rc 0 on
`agent_end`, rc 1 on error/timeout, rc 2 on stdout closing without
`agent_end`. The client's stream reader returned on the FIRST `agent_end`
frame and reaped pi immediately.

That contract predates pi's auto-retry semantics. pi emits `agent_end` after
EVERY auto-retry attempt, not once per run: an attempt ends the run, the
retry loop emits `auto_retry_start` and starts a fresh run, and only after
the last attempt does the settle boundary run and pi emit `agent_settled`
(present in the RPC stream in pi 1.5.2 and 1.6.0; `_emitAgentSettled` in
`core/agent-session.js`).

The consequence, proven live against explabs while `org_token_rate_limit_day`
was active:

- Each supervisor round = exactly one 429 attempt (~3s). pi was SIGTERMed
  mid-backoff (2s/4s/8s) before its retry loop or the settle boundary ran.
- `agent_before_settle` — where pi-model-fallback classifies the 429 and
  switches providers — NEVER executed in supervisor rounds.
- The next round redrew the same rate-limited chain (persistent preselect),
  burned one 429, exited; the round cap fataled with zero switches.
- The identical prompt outside the supervisor (print mode, or a manually
  held RPC session) retried 3-4 times in-turn and settled onto
  `openrouter/openrouter/free` — the "fallback fires in standalone probes
  but not supervisor sessions" asymmetry the operator reported.

Secondary evidence: the plugin's `state.json` recorded
`explabs → openrouter/free` entries only from standalone probes, never from
supervisor rounds; supervisor `status` showed `models_used` stuck on
`opencode/deepseek-v4.1-flash-free, explabs/deepseek-v4.1-flash` across all
six rounds.

## Decision

The round ends on `agent_settled`, never at a retry-boundary `agent_end`:

1. **`agent_settled` closes the round.** The settle boundary has run —
   pi-model-fallback's switch included — so the round's verdict is final.
2. **A bare `agent_end` is held for `retryGrace` (5s).** If
   `auto_retry_start` arrives, the round continues (the previous end was a
   retry boundary). If the grace expires with neither a retry frame nor
   `agent_settled`, the `agent_end` was the run's final one and the round
   closes normally — pi builds that never emit `agent_settled` keep
   working. The grace costs nothing in the common case (pi emits
   `agent_settled` immediately after the final attempt) and at most 5s in
   the legacy case.
3. **Aborted and held-settle runs do not close the round.**
   `agent_settled` with `aborted: true`, or while `holdEnd`/`drain` is
   armed, is ignored: the abort-drain handshake (ADR-0007) delivers the
   held interrupt prompt into a fresh run that must not be cut off by the
   aborted run's settle frame.

The reader is restructured around a channel pump: the scanner goroutine
feeds frames to a channel so the consumer can select them against the grace
timer (`retryGrace = 5s`, far above pi's immediate post-`agent_end` retry
emission, far below the shortest retry backoff).

## Consequences

- Rate-limited rounds now survive pi's internal retry sequence and reach the
  fallback extension's settle boundary, so the configured fallback chain
  (opencode free → explabs → openrouter/free) advances within one round
  instead of stranding the job on a dead leg.
- Rounds that end in exhausted retries take as long as the retry backoffs
  (~15-30s) instead of 3s. The instant-exit strike (rc≠0 && <60s && runlog
  <4KB) is unaffected: a retried round still exits 0, and a 0-token error
  shape with no retries remains instant.
- The `fake-pi.py` fixture now emits `agent_settled` after every
  turn-ending `agent_end` (never after the abort path), mirroring real pi;
  the `TEST_RETRY` fixture drives two retry boundaries plus a final settle
  (`TestRetryBoundariesDoNotSpendBudget` in `internal/client` pins the
  contract).
- Round economics: a round that would previously cost one 429 attempt now
  costs up to `maxRetries` attempts of backoff inside one round. Jobs
  burning free-pool quota per attempt will consume it faster per round, but
  the alternative — round-capping on the same 429 with no fallback — was
  strictly worse.

## Alternatives considered

- **Native fallback chain in the supervisor** (model chain config, per-round
  failover). Rejected by the owner: pi-model-fallback already owns chain
  semantics, cooldowns, and state; duplicating them in the daemon splits one
  policy across two processes. The client bug was the only reason the
  plugin's chain never advanced.
- **End rounds on the first `agent_end` but restart pi in-process.**
  Requires the daemon to re-derive steering/bookkeeping per attempt and
  still kills the settle boundary — same bug, more machinery.
- **Disable pi-core auto-retry per round.** Removes the very mechanism that
  recovers transient throttling; the 429 storm would hit the plugin once
  instead of after retries, and every transient hiccup becomes a model
  switch.
