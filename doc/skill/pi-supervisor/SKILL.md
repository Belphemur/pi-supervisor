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
pi-supervisor logs <name> 50     # tail the run log
pi-supervisor steer <name> 'POLICY CHANGE FROM THE OWNER ...'
pi-supervisor stop <name>        # SIGTERM the client group; session kept
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
- **Steering:** appends to `/tmp/pi_<name>.ctrl`, the same file
  `pi_control.py` uses — both tools coexist.
- **State:** `~/.pi/supervisor/state/<name>.json`, written atomically
  (temp + fsync + rename). Daemon restarts adopt jobs as resumable
  (`stopped`), never auto-running.
- **systemd status:** `systemctl --user status pi-supervisor` shows
  `N parallel pi session(s) running`; it updates on each watchdog beat and
  whenever the count changes.

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
| rc=2 in the log | pi's stdout closed with no agent_end (crash mid-turn) — treated as a failure, not a clean cap |

## Pitfalls (from the bash era, still true)

- Never run the RPC client in a foreground tool call — the timeout kills it
  mid-turn. The daemon exists to make this moot.
- One steer = one coherent directive block; never re-send the whole brief.
- Steers to a *finished* job's ctrl file never land — check `status` first.
- Session JSONL is the memory: never delete it to "reset"; clear the job
  state instead.
