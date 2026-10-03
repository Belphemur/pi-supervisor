# Feature Request: daemon-native clean-session restart (`restart --fresh`)

**Repo:** pi-supervisor
**Author:** Antoine Aflalo (Belphemur), Hermes Agent
**Date:** 2026-10-03
**Evidence base:** job `lowpower-stats`, rounds 5–9 (Endurance power-stats campaign)

## Summary

Add a first-class daemon command that forces a job's next round to run in a **brand-new
pi session** — quarantining the stale transcript, clearing adopted session state, and
relaunching as a true LAUNCH (brief + cont re-read from disk) — instead of the current
silent re-adoption of a poisoned session. Today this requires a manual three-step dance
(archive the session JSONL by hand, delete the state file, `start`) that the owner had to
perform twice in one night, and getting it wrong silently burns a round.

## Problem

`FindSession` (`internal/job/job.go:381`) resolves a job's session as **the newest
`.jsonl` in the worktree's munged session dir** (`MungedSessionsDir`,
`internal/job/job.go:125`). `session_path: null` in the job JSON does not mean
"start fresh" — it means "rediscover", and rediscovery deterministically finds the same
stale transcript. Consequences observed in a single campaign (rounds 5–9):

1. **Poisoned resume.** After stopping a round that committed against a superseded spec,
   relaunch resumed the old transcript. pi re-read stale context and re-validated the
   *wrong* work (stale commits `b2fee5a0`/`543b753e` were still branch HEAD).
2. **Empty-turn stall.** A resumed session produced assistant turns with zero content and
   zero `tool_use` for 35+ seconds (`pi` pid alive, state `Sl`, `session_bytes` frozen) —
   a wasted round with no error signal. The round had to be manually stopped.
3. **Masked steer loss.** The ctrl channel acks a steer as `outcome: forwarded` the moment
   the frame is written — not when pi consumes it. Steers written while pi is inside a
   long `gh pr checks --watch` poll are never read (the ctrl file is truncated at round
   start), yet the ack claims delivery. Three rounds in a row applied a correction the
   dashboard said had been delivered.
4. **No CLI surface for the fix.** The only working recovery is manual shell surgery:
   `mv <session>.jsonl <dir>/_archived-stale/` + `rm state/<job>.json` + `start`. This is
   undocumented, races the daemon (state re-adoption can win), and archive naming is
   ad-hoc.

## Proposed behavior

### 1. `pi-supervisor restart <job> --fresh` (new command)

Atomic, ordered sequence inside the daemon (no interleaving window where discovery can
re-adopt):

1. Stop the live round if any: existing SIGTERM → SIGKILL-within-3s escalation; wait for
   `client_pid` to reap (must not return while the pi process survives).
2. **Quarantine** the session JSONL: move it to `<session-dir>/_archived-stale/` with a
   timestamp suffix (`2026-10-03T01-00-26_01a0fefe.jsonl`). Never delete — transcripts are
   the audit trail for post-mortems. Rotate quarantine dir if it exceeds a size cap
   (e.g. 500 MB, oldest-first delete).
3. Clear job state: `session_path: null`, `round` reset to 0 (fresh LAUNCH semantics —
   round counter restarts, but cumulative round history stays in the events log for audit).
4. Relaunch: spawn `pi --mode rpc` with a new session, and inject the round-1 prompt as
   **brief + cont read from disk at spawn time** (not from the old transcript tail).
5. `STATUS=restarting <job> (fresh session)` via sd_notify during steps 1–4.

Also accept `--fresh` on `start` (`pi-supervisor start <job> --fresh`) as shorthand, and
make `restart` without `--fresh` the plain stop+resume it is today.

### 2. Discovery guard (defense in depth)

`FindSession` must never re-adopt a session the daemon itself quarantined: after a
quarantine, skip `_archived-stale/` (already true since it's a subdir) **and** record the
quarantined filename in state; if discovery would return a pinned/quarantined name, fall
through to "no session — LAUNCH" instead of re-adopting. Add a `W`-level event:
`{"event":"session_readopted","file":...,"bytes":...}` so re-adoption is visible in the
events log instead of silent.

### 3. Steer read-receipts (separate but same root)

The ctrl channel's `outcome: forwarded` must be renamed to `written`. A second ack
`outcome: consumed` is emitted only when the agent actually reads the frame — the daemon
can detect consumption by watching for the reader's offset past the frame (it already
tails the session JSONL; the ctrl read happens on the same harness). The supervisor's
round log and `watch` output should surface `steer written-but-not-consumed` after N
minutes so the operator knows the frame landed in a dead window (the round-2/4/6 failure).

### 4. Empty-turn stall detection

A round whose session grows less than X bytes over Y seconds while the pi process is alive
and `tool_use` count is 0 for the whole window (configurable, default 60s / 0 tool calls)
should be classified as a stall — same escalation path as `ci_stall` (interrupt →
re-prompt → escalate), instead of burning the full `timeout_s`.

## Acceptance criteria

- [ ] `pi-supervisor restart lowpower-stats --fresh` on a running job: old pi process
      reaped, transcript moved into `_archived-stale/` with timestamp suffix, state
      `session_path: null`, round counter 0, new session UUID in a fresh `.jsonl`, and the
      round-1 prompt content equals the on-disk brief + cont files.
- [ ] Re-running `--fresh` twice in a row never re-adopts the just-quarantined file.
- [ ] Quarantine preserves the transcript byte-for-byte (sha256 recorded in the events log).
- [ ] `start --fresh` behaves identically to `restart --fresh`.
- [ ] Steer acks report `written` immediately and `consumed` only on actual read; the
      round log prints a visible warning when a written steer goes unconsumed for >2 min.
- [ ] Empty-turn stall (0 tool_use, sub-threshold growth, pi alive) triggers the stall
      classifier and is visible in `watch` output with the same schema as `ci_stall`.
- [ ] `go vet ./...` and `go test ./...` green; new behavior covered by unit tests for the
      quarantine ordering (stop → quarantine → clear → adopt-none → spawn).
- [ ] `doc/` gains an ADR describing the state machine (ADR-0005: clean-session restart)
      and the skill file documents `restart --fresh` in the recovery section.

## Non-goals

- Editing or rewriting session transcript contents (quarantine is move-only).
- Multi-session concurrency per job (one live session per job remains the model).
- Changing the ctrl file protocol shape beyond ack semantics (consumers of `watch` keep
  their schema; new events are additive).