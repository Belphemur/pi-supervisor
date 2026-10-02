---
name: pi-supervisor
version: 1.0.0
author: Antoine Aflalo (Belphemur), Hermes Agent
license: MIT
platforms: [linux]
description: "Supervise long-running pi delegations via a systemd Go daemon with a queryable control socket."
metadata:
  hermes:
    tags: [Coding-Agent, Pi, Orchestrator, Long-Running, Supervision, systemd]
    related_skills: [pi-orchestrator, pi, answer-code-review, agent-delegation-pitfalls]
---

# pi Supervisor Daemon (Go + systemd)

`pi-supervisor` is a Go 1.27 daemon (systemd `--user`, `Type=notify`, watchdog)
that runs the pi RPC resume loop for named jobs: spawns `pi_rpc_client.py`
rounds, resumes the same session JSONL across client caps, classifies deaths,
backs off adaptively, and answers queries over a Unix socket. It replaces the
bash `pi_supervisor.sh` for anything that must outlive the launching agent.

Canonical source + docs live in `/home/balor/workspace/pi-supervisor`
(`AGENTS.md`, `doc/adr/`, `doc/skill/pi-supervisor/` — this skill is a symlink
into that doc folder, so edit it there).

## When to use

- Multi-hour pi campaigns that must survive agent lifecycle teardown, context
  compression, or a Hermes restart.
- Any run where you want to ASK the supervisor what's happening
  (`pi-supervisor status`) instead of grepping session files.
- Do NOT use for short one-shot delegations (the `pi` skill suffices).

## Quick start

```bash
# Install (builds, installs unit, symlinks this skill, enables service)
/home/balor/workspace/pi-supervisor/install/install.sh

# Define a job: ~/.pi/supervisor/jobs/<name>.json (see AGENTS.md template)
pi-supervisor reload
pi-supervisor start <name>       # round 1 LAUNCHes, later rounds RESUME

# Operate
pi-supervisor status             # all jobs: state, round, session size/age
pi-supervisor status <name>      # one job
pi-supervisor logs <name> 50     # tail the run log (one line per log line)
pi-supervisor steer <name> 'POLICY CHANGE FROM THE OWNER ...'
pi-supervisor steer <name> -n '...'  # don't wait for pi's ack
pi-supervisor stop <name>        # SIGTERM the client group; session kept

# Notifications: block until the supervisor sends an event (see below)
pi-supervisor watch <name>       # exit after the first event
pi-supervisor watch <name> -t    # exit only when the run is over (done/fatal/stopped)
```

## Architecture

The daemon spawns `pi --mode rpc` **directly** — no Python. `internal/client`
is a Go port of the old `pi_rpc_client.py`: LF-delimited JSON framing, streamed
text capture, control-file steering, the abort-drain handshake, and
timeout/abort/SIGTERM/SIGKILL escalation. Exit codes the supervisor classifies:
`0` agent_end, `1` error/timeout/spawn failure, `2` stdout closed with no
agent_end.

```
systemd (Type=notify, WatchdogSec=120)
└── pi-supervisor run
    ├── control socket  $XDG_RUNTIME_DIR/pi-supervisor.sock  (JSONL, 0600)
    ├── notify.Beat     WATCHDOG=1 + STATUS=<n> parallel pi session(s)
    └── per job: round loop → internal/client → pi --mode rpc
```

## Semantics

- **Round loop:** client `--timeout 1800`; supervisor backstop kill at +120s;
  adaptive backoff (clean cap 15s / long run 20s / instant implosion 90s /
  else 30s), multiplied by the job's optional `backoff_scale`.
- **Session rule:** session path captured once after round 1 LAUNCH; every
  later round RESUMEs `--session <path>`. Never re-LAUNCH (forks the session).
- **Instant-exit strikes:** rc≠0 && <60s && runlog <4KB ⇒ strike; 3 strikes ⇒
  job `fatal` (context wall / model refusal — needs operator action).
- **Completion:** final report exists AND marker in the run log's last 4KB ⇒
  job `done`.
- **Steering:** wraps prose in a `{"type":"prompt","message":...}` frame
  (a hand-written JSON frame with a `type` field passes through unchanged);
  each frame gets an id (`steer-<ns>-<seq>`) written to the job's control
  file (`~/.pi/supervisor/control/<name>.ctrl` — the same file
  `pi_control.py` uses, so both tools coexist). The client acks what it did
  with each frame in `~/.pi/supervisor/events/<name>.ack.jsonl`, and `steer`
  prints a factual report: job, round, session path, frame id, live-round +
  client pid, and the outcome (`forwarded` / `held` then delivered /
  `send failed` / `bad frame` / `no live round`). It blocks until that ack
  (bounded by the job's `timeout_s`, default ~20s) and exits non-zero unless
  pi took the frame. `-n` skips the wait and reports only `written`. See
  ADR-0005.
- **State:** `~/.pi/supervisor/state/<name>.json`, written atomically
  (temp + fsync + rename). Daemon restarts adopt jobs as resumable
  (`stopped`), never auto-running.
- **systemd status:** `systemctl --user status pi-supervisor` shows
  `N parallel pi session(s) running`; it updates on each watchdog beat and
  whenever the count changes.

## Notifications — the `watch` command (ADR-0003)

The session that started a job gets told what happened — no cronjob, no
polling, no configured chat address. Correctness is structural: the watch is a
blocking client over the control socket, armed as a background run, and the
background-completion notification is delivered **to the session that armed
it**. It cannot go anywhere else.

```
# Arm (Hermes): a background process that wakes this session on the next event
terminal(command="pi-supervisor watch power-top", background=true, notify=true)

# Or hold ONE process for the whole campaign; it wakes only when the run is over
terminal(command="pi-supervisor watch power-top -t", background=true, notify=true)
```

Behavior:

- **Blocks silently** until the supervisor pushes an event. Events:
  `job_started`, `round_done` (rc, duration, output tail), `instant_exit`
  (strike n/3), `backoff` ("not dead, sleeping Ns"), `ci_stall` (the agent
  parked on the CI/answer-code-review loop — see ADR-0004: the daemon tails
  the session JSONL and at `ci_stall_cap` parks interrupts the session with a
  finish-the-report prompt and closes the run as a review-loop failure), and
  the terminals `done` / `fatal` / `stopped`.
- **Finished run ⇒ immediate return.** If the job is already done/fatal/stopped
  when you arm, the watch does NOT block — it prints
  `THE RUN IS OVER — <job> <event>`, points at `status`, and says not to re-arm.
- **Every exit message is self-explanatory** (owner requirement): after a
  non-terminal event it prints the re-arm command (`pi-supervisor watch <job>`,
  background+notify) and the status command; after a terminal event it says the
  run is over and that no re-arm is needed. `stopped` additionally prints the
  resume command. Act on the event, then re-arm for the next one.
- **Exit codes:** 0 = event delivered (or run already over), 1 = connection
  lost (daemon restarted — check `systemctl --user status pi-supervisor`,
  then re-arm), 2 = usage error (unknown job).
- **Transcript tail.** Every exit message first prints the last complete lines
  of the session JSONL (`session_path`, 2 lines, 300 chars each) plus where the
  rest lives. A live transcript is being appended to while it is read, so only
  newline-terminated lines are shown — a torn trailing write is never printed.
- Do NOT arm a watch in a foreground tool call — the 600s cap kills it mid-wait.
  Background+notify is the pattern; a foreground call is only for probing.
- Every event is also appended to `~/.pi/supervisor/events/<job>.jsonl`
  (append-only audit trail, human-greppable). Nothing reads it automatically.

## systemd integration

- Unit: `~/.config/systemd/user/pi-supervisor.service`, `Type=notify`,
  `Restart=on-failure`, `WatchdogSec=120` (daemon pings `WATCHDOG=1` at
  half-interval via pure-Go sd_notify).
- Check readiness with `systemctl --user is-active` — `active` means READY=1
  was accepted, not merely that the process exists.
- `journalctl --user -u pi-supervisor -f` for the daemon's own log.

## Troubleshooting

| Symptom | Meaning / action |
|---|---|
| job state `fatal`, diag "3 consecutive instant exits" | session context wall — start a fresh session (new job or clear state) or trim the session |
| `start` says "already done" | marker+report were reached; clear `~/.pi/supervisor/state/<name>.json` to rerun |
| ctl: "daemon not reachable" | `systemctl --user status pi-supervisor`; journal for socket errors |
| `systemctl status` count is stale | the beat only rewrites STATUS when the count changes; a count that never moves means no round is ending |
| `steer` says `no live round` / `not confirmed` | no round was polling the ctrl file, or pi never acked within the bounded wait (~20s) — the frame was NOT delivered; check `status` and the run log, then re-send |
| rc=2 in the log | pi's stdout closed with no agent_end (crash mid-turn) — treated as a failure, not a clean cap |

## Pitfalls (from the bash era, still true)

- Never run the RPC client in a foreground tool call — the timeout kills it
  mid-turn. The daemon exists to make this moot.
- One steer = one coherent directive block; never re-send the whole brief.
- **Steering blocks, it does not queue.** `steer` waits for the client's ack
  (bounded by the job's `timeout_s`, default ~20s) and exits non-zero unless
  pi took the frame; `-n` skips the wait and reports only `written`. A steer
  with no live round reports `no live round` — the ctrl file is truncated at
  every round start, so a queued frame would be wiped unread. Wait for
  `status` to show a round, then re-send.
- Session JSONL is the memory: never delete it to "reset"; clear the job
  state instead.
