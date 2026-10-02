# ADR 0003 — daemon-to-Hermes session notifications

Date: 2026-10-02
Status: Accepted — blocking `watch` command; no cronjob, no pulling
Author: Antoine Aflalo (Belphemur), Hermes Agent

## Context

The owner wants the daemon to notify the Hermes session that started a pi job —
round completions, new output, done/fatal — so the session becomes aware of
work to check or work done. Owner requirements, verbatim in spirit:

1. The notification MUST land in the **right existing chat session** — the one
   that started the job. Never somewhere else.
2. A command the agent runs and **gets stuck waiting** on until the supervisor
   sends data — e.g. as a background run.
3. **No cronjob pulling.**
4. When the watch ends and the run is over, it must say so directly.
5. The exit message must tell the agent how to get status and/or continue
   watching.

The daemon is a systemd --user process with no chat credentials; it cannot
post to Signal/Telegram itself.

## Decision

**A blocking `watch` command over the control socket — the only mechanism.**

```
pi-supervisor watch [job]        # exit after the first event
pi-supervisor watch [job] -t     # (--terminal) exit only on done/fatal/stopped
```

- The client connects to the control socket, sends `{"cmd":"watch",
  "job":"power-top"}` and blocks. The daemon pushes each lifecycle event as a
  JSON line over the held-open connection.
- **Already-over check (requirement 4):** on connect, the daemon first checks
  the job's current state. If it is `done` or `fatal`, the watch returns
  immediately with "the run is over" — it never blocks on a finished job.
- **Self-explanatory exit (requirement 5):** after the event the client prints
  a footer saying exactly what to run next:
  - non-terminal event → `re-arm: pi-supervisor watch <job>` (background,
    notify) and `status: pi-supervisor status <job>`
  - terminal event → "THE RUN IS OVER" + status pointer + "do not re-arm".

- **Right-session guarantee (requirement 1):** the session arms the watch with
  its native background pattern:

  ```
  terminal(command="pi-supervisor watch power-top", background=true, notify=true)
  ```

  The background-process completion notification is delivered **to the session
  that started that process** — the original chat, by construction. Correctness
  comes from the agent's own background-notify plumbing, not from any
  configured address. The session wakes, handles the event, and re-arms a
  fresh watch.
- Exit codes: 0 = event delivered (or run already over), 1 = connection lost
  (daemon restarted — wake and re-arm), 2 = usage error. A watch armed on a
  healthy job simply blocks until the next event; arm it as a background run
  so the foreground timeout cap never applies.

### Why not a delivery cronjob? (requirement 3)
Rejected. A cronjob's `deliver` target is its own creation context; a watcher
cronjob can post to a chat other than the job's starting session, and polling
adds latency. The watch command has neither failure mode: the notify goes to
whoever armed the watch, and events are pushed within seconds.

## Event set
- `job_started`  — first round launched.
- `round_done`   — one RPC round ended; carries rc, duration, short text tail.
- `instant_exit` — rc≠0 and the run lasted <60s (potential context wall).
- `backoff`      — seconds sleeping before the next round ("not dead, backing
  off").
- `done`         — marker + final report detected (terminal).
- `fatal`        — round cap or 3 instant exits (terminal; operator action).
- `stopped`      — operator stop (terminal for the watch; job is resumable).

## Wire protocol (control socket)

Request: `{"cmd":"watch","job":"<name or empty=all>"}`.
Server replies immediately `{"ok":true,"data":{"watching":"<job|*>"}}`, then
streams `{"ok":true,"data":{<Event>}}` per event, closing the connection after
a terminal event (or when the client disconnects). Events are fanned out from
the round loop through an in-process broker (buffered, non-blocking send — a
slow or gone subscriber can never stall a round loop).

## Consequences

- The daemon never holds credentials and never addresses a chat; the agent's
  background-notify delivers to the session that armed the watch, always.
- Real-time, not batched: the session wakes within seconds of the event.
- Daemon restart breaks watches: armed clients exit 1 and the session re-arms
  after restarting the daemon. State is re-checked on the next connect, so an
  over-finished job returns "run is over" instead of blocking.
- Every event is also appended to `~/.pi/supervisor/events/<name>.jsonl`
  (append-only) as a debugging/audit trail for humans; nothing polls it.
