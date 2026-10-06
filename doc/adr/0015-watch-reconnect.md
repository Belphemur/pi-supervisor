# ADR 0015 — watch survives a daemon restart

Date: 2026-10-06
Status: Accepted
Author: Antoine Aflalo (Belphemur), Hermes Agent

## Context

ADR-0003's `watch` is the only delivery path from the daemon to Hermes (no
cronjob, no polling). Its weakness showed on 2026-10-06 with the
`mealime-restrictions` job (flambette PR #51): the armed watch was a plain
one-shot (`watch <job>`, no `-t`), which by design exits after its FIRST
event — a `round_done`/`backoff` — and the `fatal` that fired 34 seconds after
the last backoff had no listener left. `status` said `fatal`; the armed watch
never said anything.

Two distinct failure classes produce "status says fatal, the watch said
nothing":

1. **One-shot semantics** — a plain `watch` exits on the first event and the
   arming session must re-arm. Miss the re-arm and every later event,
   including the terminal one, is unwitnessed. This is by design (ADR-0003);
   the operational fix is `watch -t`.
2. **A severed live stream** — the daemon restarted between arming and the
   terminal event. The client exited 1 ("watch connection lost") and never
   came back. The daemon was restarted four times that day (install.sh and
   operator bounces); any `-t` watch alive across one of those restarts died
   exactly when its one job — delivering the ending — was still ahead of it.

Class 2 is a code defect, not an operational one: the client holds a stream
worth recovering, and the daemon already has the machinery to tell a
reconnecting client exactly what it missed.

## Decision

**A `-t` (and plain) watch whose stream was LIVE — the daemon answered
`watch_ack` — reconnects with bounded backoff and re-issues the watch
request.** Recovery rides the existing ADR-0003 machinery:

- On reconnect the daemon re-derives state server-side (`Watch`'s precheck).
  If the job went terminal during the gap, the precheck delivers the missed
  terminal event immediately and the client prints the normal exit footer and
  exits 0. If the job is running again, the client resumes streaming.
- Schedule: 2s, 5s, 10s, 20s, 30s (~67s ceiling), hardcoded — it has no user
  (systemd `Restart=on-failure` brings the daemon back in ~1s; changing it is
  an ADR amendment, not a config knob). A successful `watch_ack` resets the
  attempt counter, so a client that survives many restarts never exhausts a
  shared budget.
- Progress is printed to stderr ("reconnecting in 2s (attempt 1/5)") — a
  silent wait is indistinguishable from a hang.

**A connection that never answered is NOT a restart.** EOF (or dial refusal)
before any `watch_ack` keeps the old behavior — exit 1 with the re-arm
footer, immediately. A daemon that accepts and hangs up is broken, not
restarting; retrying it would only delay the failure report.

**Exit contract (ADR-0008) unchanged.** 0 = event delivered (terminal event,
or first event for a plain watch, or missed-terminal precheck after
reconnect). 1 = stream severed AND the reconnect budget exhausted (or a
connection that never answered). 2 = usage error / daemon not reachable on
the FIRST connect.

**Invariant 6 (push-only) is preserved.** Reconnect is not polling: the
client re-subscribes once and the daemon pushes what it re-derived. Events
lost during the gap stay lost (the audit JSONL keeps them for humans) — only
the CURRENT state is re-derived, which is exactly the precheck's existing
contract for a client that arms late.

## Consequences

- A `watch -t` armed as a background+notify run now survives the daemon
  restarts that killed it before; the missed-fatal class is closed for live
  streams. It still does NOT survive its own session teardown — arming with
  `persist_on_release` remains the operator's job.
- One-shot watches (no `-t`) gain the same reconnect for the pre-event window
  only: they still exit on their first event. The re-arm discipline of
  ADR-0003 is unchanged.
- The unknown-job error still exits 2 on the first connection and is never
  retried — a bad job name would otherwise burn the whole budget.

## Non-goals

- Replaying events missed during the gap. The broker is in-memory; the audit
  JSONL is the record. Only the missed TERMINAL state is recoverable, via the
  precheck.
- Surviving session teardown of the arming session (platform behavior, not
  the daemon's).
- Making the schedule configurable. No deployment varies it.
