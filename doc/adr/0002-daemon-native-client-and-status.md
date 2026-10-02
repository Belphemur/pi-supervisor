# ADR 0002 — Daemon-native RPC client + parallel-session status

Date: 2026-10-02
Status: Accepted — implemented and deployed 2026-10-02
Author: Antoine Aflalo (Belphemur), Hermes Agent

## Context

1. `pi-supervisor` currently spawns `pi_rpc_client.py` (200 lines of stdlib Python)
   to bridge the daemon to `pi --mode rpc` framing, control-file polling, and
   timeout management. The owner wants the daemon self-contained — no Python.
2. systemd has no visibility into how many pi sessions run in parallel, which
   matters for resource accounting and for catching the "daemon alive but
   every round is instant-exit" failure class.

## Decision

- **Fold the RPC client into the daemon** as `internal/client`:
  - subprocess management of `pi --mode rpc` (stdin JSON frames, stdout line stream)
  - event correlation: `message_update` (text delta), `agent_end` (done),
    `response` (error), mirroring the Python reader's semantics
  - **abort-drain protocol**: on timeout, send `{"type":"abort"}`, then hold
    subsequent control frames until the aborted turn's `agent_end` arrives,
    then flush them (same `hold_agent_end`/`drain_wait` handshake)
  - control-file polling at `/tmp/pi_<name>.ctrl` → forward new JSON lines to
    pi's stdin with the same held-frame-on-abort behavior
  - 1800s deadline (job-controlled), abort → 30s grace → SIGTERM/SIGKILL
- **Parallel-session count to systemd**: on every notify beat (currently the
  watchdog pinger's cycle) emit
  `STATUS=<n> parallel sessions running` alongside `WATCHDOG=1`, where n is the
  count of jobs in `running` state. One `sd_notify` syscall per beat carries both.
- **Control socket parity**: `steer` continues to append to the same `.ctrl` path
  so `pi_control.py` and the daemon-native client interoperate during cutover;
  a future task may retire `pi_control.py` too.

## Non-goals

- Rewriting `pi` itself. The daemon owns subprocess life; `pi --mode rpc` stays
  the substrate.
- Changing the control-socket command set or job schema.

## Implementation notes (what actually shipped)

- `internal/client/client.go` — the port. Notable details the Python version
  did not have: a `stream` struct owning the run-log `bufio.Writer` under one
  mutex (the flusher goroutine and the event reader must not race), an
  `OnPID` callback so the supervisor can still kill pi's process group, and a
  graded reap grace (5s on the timeout path, 10s on the clean path) instead of
  a flat 15s wait.
- Exit codes are richer than the Python original's 0/1: `2` was added for
  "stdout closed with no agent_end" (pi crashed mid-turn), which is a failure
  but not the same failure as an explicit error response.
- `FindSession` lost its `pi_session.py` subprocess, so the daemon now spawns
  **no Python at all**.
- `internal/job` gained `writeAtomic` (temp + fsync + rename) after noticing
  the old best-effort `os.WriteFile` could lose the last transition on a crash,
  plus a `backoff_scale` job knob (tests need sub-second round cycles; the
  90s instant-exit backoff is otherwise untestable in a unit test).
- `notify.Beat` replaces `notify.Watchdog`: one datagram carries both
  `STATUS=<n> parallel pi session(s) running` and `WATCHDOG=1`, and the write
  is skipped when the count is unchanged to avoid a pointless syscall per beat.

## Testing

`testdata/fake-pi.py` is a fake `pi --mode rpc` that speaks the real protocol
and selects behavior from the prompt (`TEST_STREAM`, `TEST_ERROR`,
`TEST_NOEND`, `TEST_HANG`, `TEST_SLOW`, `MARKER_*`). 19 tests, all race-clean:

- client: framing, rc 0/1/2 classification, spawn failure, timeout escalation,
  control-file steering, streaming to the run log, arg construction
  (resume ⇒ `--session` and never `-n`; launch ⇒ `-n` and repeated `--skill`),
  and the abort-drain handshake (a frame arriving mid-drain is held, the
  aborted turn's `agent_end` must NOT end the round, then the held frame is
  released).
- supervisor: round cap, marker gate, instant-exit strikes, running count,
  state adoption across reload, stop leaves resumable state, steering writes
  the ctrl file, atomic state writes leave no temp files, and the never-fork
  guard.

Two real bugs the tests caught, both fixed: a data race between the run-log
flusher and the event reader, and a nil-deref when the stream had no writer.

## Consequences

- `pi_rpc_client.py` is no longer used by the daemon. It stays in the `pi`
  skill for ad-hoc manual delegations.
- systemd gains a live session count in `systemctl status pi-supervisor` and as a
  `STATUS=` in the journal on each watchdog beat.
- Abort-drain parity is preserved bit-for-bit; the daemon's exit classifier sees
  the same rc 0 (agent_end) / 1 (timeout or error) signals.
