# ADR 0005 — Steer acknowledgements: every steer reports a real outcome

Date: 2026-10-03
Status: Accepted
Author: Antoine Aflalo (Belphemur), Hermes Agent

## Context

`pi-supervisor steer` had three defects that all pointed the same way: the
CLI claimed success for things it had not verified.

1. **Prose was silently dropped.** The documented usage
   (`pi-supervisor steer <name> 'POLICY CHANGE FROM THE OWNER ...'`) appended
   the raw line to `/tmp/pi_<name>.ctrl`, but `controlReader.pump` parses
   every line as JSON and skips anything else with
   `[control] bad frame skipped`. The operator got exit 0 and an empty
   effect. The unit test passed only because it fed a pre-formatted JSON
   frame — the one thing an operator never types.
2. **A steer to an idle job was wiped.** `round()` truncates the ctrl file at
   the start of every round (that is what makes the file a per-round steer
   channel), so anything written while no round was polling it disappeared
   unread on the next round.
3. **No delivery feedback.** Even a well-formed frame had no observable
   result: the operator could not tell "forwarded to pi's stdin" from "held
   behind an abort drain" from "pi's stdin rejected it" from "the round ended
   first". During the endurance campaign a steer sent into a dying round looked
   exactly like one that had landed.

## Decision

**A steer is a request with an id; the client acks every frame it acts on, and
the CLI prints the ack.**

- **Frames, not prose, on the wire.** `Steer` wraps prose in
  `{"id":…,"type":"prompt","message":…}`; a caller-supplied JSON object with a
  `type` is passed through untouched (hand-written aborts keep working), and
  anything else — including JSON that is not a frame — is prose. The client's
  strict LF-JSON contract is unchanged: it still refuses to guess.
- **Unique id per frame.** `steer-<unixnano>-<seq>`, or the caller's own id.
  It is the correlation key for the ack.
- **Ack channel: `/tmp/pi_<name>_ack.jsonl`** (`job.Ack`), truncated together
  with the ctrl file at every round start. The client appends one
  `job.AckRecord{ID, Outcome, Type, Detail, DelayMS, AtMS}` per frame it acts
  on: `forwarded`, `held`, `send failed`, `bad frame`. One `write(2)` per
  record, so a record is durable the instant it is written; the client never
  blocks on it (an unusable path is dropped after the first failure rather
  than breaking steering).
- **`held` is not terminal.** A frame queued behind an in-flight abort gets a
  `held` record, then a second `forwarded` record when the drain releases it,
  carrying how long it waited. `steer` waits for the terminal record and
  reports the hold explicitly.
- **Bounded wait, no locks.** `steer` waits at most one poll interval plus a
  2s margin (the client's poll is 5s), then reports `not confirmed` and exits
  non-zero. It takes no lock across the wait: the snapshot is taken under
  `r.mu`, released, and the round loop is never blocked. The wait ends early if
  the ack log shrinks below the offset it started from — that is the round
  restarting, and it is reported as *not delivered*, not as a timeout.
- **No live round ⇒ nothing written.** Steering a job with no running round is
  an error with the report attached (`no live round`, with state, round and
  ctrl path), because queueing there would only be truncated. The old
  behaviour — write and hope — is the bug.
- **The report is the output.** `steer` prints job, round, live-round flag, pi
  pid, session path, ctrl path, frame id, ack path, outcome, delay, and how
  long it waited, then exits 0 only when pi acknowledged the frame. `-n` /
  `--no-wait` skips the wait and honestly reports `written`.
- **Read the ctrl file from the start.** The reader used to position itself at
  the file's current end, which silently dropped a steer written between the
  round's truncate and the client's spawn. The file is truncated per round, so
  its entire contents belong to the current round; a failed truncate is now a
  round failure instead of a `_ =` no-op.

## Consequences

- `steer` is honest: exit 0 means pi took the frame.
- The ack log is a fifth per-job `/tmp/pi_<name>_*` file. Existing names and
  semantics are unchanged, so `pi_control.py` and orchestrator greps keep
  working; the ack file is advisory and can be deleted at any time.
- `steer` blocks for up to ~7s. That is fine at the socket layer (one
  goroutine per connection) and is bounded by the poll interval.
- The wait budget is derived from the client's poll interval instead of a
  second, independently drifting constant.

## Alternatives considered

- **Have the CLI poll the run log** for `[control] forwarded:`. Rejected: the
  run log is truncated per round and carries no frame id, so a concurrent
  steer cannot be told apart from this one.
- **Answer synchronously in the client's pump** (e.g. a unix socket back to
  the supervisor). Rejected as far more machinery for the same information —
  the ack file already exists per job and needs no lifecycle.
- **Keep writing to an idle job's ctrl file** so the next round picks it up.
  Rejected: round() truncates at start, and a steer meant for *this* round
  landing in *the next* one is worse than an explicit error.