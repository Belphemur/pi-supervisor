# ADR-0010: Clean-session restart (`restart --fresh`) and empty-turn detection

## Status

Implemented for `restart --fresh` (sub-feature 1) and empty-turn stall
detection (sub-feature 4). Sub-feature 2 (re-adoption event) is satisfied by
construction and is documented below. **Sub-feature 3 (steer `consumed`
read-receipt) is deliberately NOT implemented** — see "Rejected" at the end.

## Context

Evidence base: job `lowpower-stats`, rounds 5-9 (Endurance power-stats
campaign).

`FindSession` resolves a job's session as the newest `.jsonl` in the worktree's
munged session dir. `session_path: null` therefore means "rediscover", not
"start fresh", and rediscovery deterministically finds the same stale
transcript. Observed consequences:

1. **Poisoned resume** — pi re-read stale context and re-validated superseded work.
2. **Empty-turn stall** — a resumed session produced assistant turns with zero
   content and zero `tool_use` for 35+s (pi alive, `session_bytes` frozen): a
   wasted round with no error signal, burning the full `timeout_s`.
3. **Masked steer loss** — see "Rejected".

Recovery was manual three-step shell surgery (`mv` the JSONL, `rm` the state,
`start`), performed twice in one night and racy against session re-adoption.

## Decision

### 1. `restart <job> --fresh` — atomic stop → quarantine → clear → start

`Supervisor.Restart(name)` performs, in order:

1. **Stop** the live round. `Stop()` already escalates SIGTERM → SIGKILL within
   3s and never returns while `pi` survives. "not running" is not a failure for
   a restart — it proceeds to clear state.
2. **Quarantine** the transcript. `job.Quarantine` `os.Rename`s the JSONL into
   `<session-dir>/_archived-stale/<stem>_<2006-01-02T15-04-05>.jsonl`, with a
   collision-suffix loop for same-second moves. Move-only, never delete:
   transcripts are the post-mortem audit trail, and the bytes are preserved
   exactly (`os.Rename`, no rewrite).
3. **Clear** `job.SessionPath` (persisted via `job.Save`) and reset
   `state.Round` to 0 (persisted via `job.SaveState`). Cumulative round history
   stays in the events log for audit.
4. **Start** a fresh round. With `SessionPath == ""` the round LAUNCHES (never
   re-LAUNCHes an existing session, invariant 1) and `client.DefaultPromptFile`
   reads **brief + cont from disk at spawn time**, not from the old transcript.

An `restart_fresh` event is emitted with the quarantine path.

`restart` without `--fresh` is the plain stop+start it is today, dispatched in
`control.dispatch`. The CLI exposes `--fresh` as a flag on `restart` only; the
`shorthand on start` from the request was dropped because `start --fresh` and
`restart --fresh` would be two spellings of one operation with different
failure modes (a `start` on a *running* job already errors "already running",
so the shorthand could not work without silently becoming a restart).

### 2. Re-adoption guard

`FindSession` skips subdirectories, and the quarantine target *is* a
subdirectory, so a quarantined transcript is structurally unreachable by
discovery — no extra state field is needed. This is load-bearing: do not
"simplify" `Quarantine` to write into the session dir itself, or the next
discovery re-adopts the file just quarantined. `TestRestartFreshDoesNotReAdoptQuarantined`
pins this.

### 4. Empty-turn stall detection

`stall.Detector.EmptyTurn(EmptyTurnWindow)` fires when **both** hold for
`empty_turn_idle_s` (default 60s):

- the transcript has not grown by `MinGrowth` bytes, **and**
- no `tool_use` marker was seen in that window.

Both conditions are required because growth *without* a tool call is normal
work (the model streaming prose). The pairing is what separates "thinking"
from "wedged". `EmptyTurn` re-`stat`s the file rather than trusting the last
`Poll`, so the question is answered independently of the CI-stall detector's
cadence.

`Supervisor.watchEmptyTurn` runs alongside `watchCIStalls`: the **first**
detection emits a W-level `empty_turn` event; a **second consecutive** window
escalates on the same abort+prompt path as the CI-stall cap
(`interruptWith`), so a wedged turn is interrupted and re-prompted rather than
waited out. Same event schema as `ci_stall`, additive only.

## Consequences

- The manual three-step recovery dance is now one atomic daemon-side sequence
  with no window where discovery can re-adopt between quarantine and clear.
- Transcripts are never deleted; `_archived-stale/` accumulates. Rotation is
  **not** implemented (the request proposed a 500MB oldest-first cap) — it is
  left as an explicit non-goal rather than shipped untested, and the operator
  can prune the directory by hand.
- An empty-turn stall now costs ~2 idle windows instead of a full `timeout_s`.
- `restart --fresh` on a job whose `final_report` already exists can re-enter
  the marker gate; that is the same behavior as `start`, unchanged.

## Rejected: steer `consumed` read-receipts (sub-feature 3)

The request asks to rename the ctrl ack `forwarded` → `written` and emit a
second `consumed` ack "detected by watching for the reader's offset past the
frame". The rename is honest but the `consumed` ack **cannot be implemented as
described**: the ctrl file is read by the supervisor's own client goroutine
(`internal/client/controlReader`), not by pi. The ack is recorded after the
client writes the frame to pi's stdin — there is no later observable event that
distinguishes "pi buffered it" from "pi is blocked in a `gh pr checks --watch`
subprocess and will read it after this turn ends". A `consumed` ack derived
from the ctrl reader's offset would be true by construction and would
reintroduce exactly the false-delivery signal ADR-0005 exists to eliminate.

The underlying problem (steers landing in a dead window) is real, and the
honest fix is the one already shipped: `steer --interrupt` (ADR-0007) makes the
frame the *next* thing pi works on, and the report states the signal was sent,
never obeyed.

## Non-goals

- Editing or rewriting transcript contents (quarantine is move-only).
- Multi-session concurrency per job.
- Changing the ctrl frame protocol beyond ack semantics.