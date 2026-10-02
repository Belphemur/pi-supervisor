# ADR 0002 — Daemon-native RPC client + parallel-session status

Date: 2026-10-02
Status: Accepted (implementation follows)
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

## Consequences

- `pi_rpc_client.py` becomes dead code; remove it (the skill no longer ships it).
- systemd gains a live session count in `systemctl status pi-supervisor` and as a
  `STATUS=` in the journal on each watchdog beat.
- Abort-drain parity is preserved bit-for-bit; the daemon's exit classifier sees
  the same rc 0 (agent_end) / 1 (timeout or error) signals.
