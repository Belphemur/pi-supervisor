# AGENTS.md — pi-supervisor

Project: Go 1.27 daemon that supervises long-running [pi](https://github.com/withpi/pi)
RPC delegations (used by the CrossPoint/XPoint agent campaigns). It replaces the
bash `pi_supervisor.sh` resume loop with a systemd `--user` service that survives
agent lifecycle teardown, monitors session growth, and answers queries over a
Unix socket.

## Layout

| Path | Role |
|---|---|
| `cmd/pi-supervisor/main.go` | Entry: `run` (daemon) + ctl subcommands (status/start/stop/steer/logs/reload) |
| `internal/notify/sd.go` | Pure-Go `sd_notify`: `READY=1`, `STOPPING=1`, and `Beat()` which pairs `WATCHDOG=1` with `STATUS=<n> parallel pi session(s) running`. No cgo. |
| `internal/job/job.go` | Job model (`~/.pi/supervisor/jobs/*.json`), state persistence, session discovery, run-log helpers |
| `internal/client/client.go` | The pi RPC client, in Go: LF-JSON framing, streamed text, control-file steering, abort-drain handshake, timeout/abort/reap escalation. No Python. |
| `internal/supervisor/supervisor.go` | Round loops: drive `internal/client`, classify exits, adaptive backoff, marker gate, instant-exit strikes, never-fork guard |
| `internal/events/events.go` | Lifecycle events: append-only audit JSONL per job + in-process fan-out broker (buffered, never blocks the round loop) |
| `internal/stall/stall.go` | CI/review stall detector (ADR-0004): tails the session JSONL for CI-wait markers; stall = marker + idle window; drives the finish-the-report intervention at the cap |
| `internal/control/control.go` | Unix-socket server: one-shot request/response + streaming `watch` (pushes events, closes on terminal) |
| `install/install.sh` | Build + systemd unit + Hermes skill symlink + verification |
| `install/pi-supervisor.service` | `Type=notify` user unit (`WatchdogSec=120`) |
| `doc/adr/` | Architecture decision records |
| `doc/skill/pi-supervisor/` | Hermes skill (canonical copy; skills dir symlinks here) |

## Invariants (do not break)

1. **Never re-LAUNCH a session.** After the first round captures the session
   JSONL path, every later round RESUMEs it (`--session`). A second LAUNCH
   forks the session and splits the work. `loop()` enforces this via the
   `pinned` check; keep it.
2. **Instant-exit strike rule.** `rc != 0 && duration < 60s && runlog < 4KB`
   three times in a row ⇒ job goes `fatal` and the loop stops. This is the
   context-exhaustion death-spiral guard; do not turn it into an infinite
   retry.
3. **Process groups.** The client is spawned with `Setpgid: true`; stop/kill
   paths always signal `-pid` (the whole group), never just the pi pid.
4. **sd_notify contract.** `READY=1` only after the control socket listens and
   jobs are loaded; `STOPPING=1` before killing clients on SIGTERM. The unit
   is `Type=notify` + `WatchdogSec=120` — a daemon that stops answering the
   watchdog gets restarted by systemd, which is the "doesn't die" guarantee.
5. **Per-job file names are load-bearing** (`/tmp/pi_<name>.ctrl`,
   `/tmp/pi_<name>_run.log`, `_orchestrator.log`, `_status.json`): external
   tooling (`pi_control.py`, orchestrator greps) depends on them.
6. **Watch is push-only, never pulled.** `cmd:"watch"` holds the connection
   and streams events from the in-process broker; the broker's send is
   non-blocking (drop, don't stall the round loop). A finished job answers
   immediately with a precheck event instead of blocking, and the client's
   exit message must always say how to re-arm / get status (ADR-0003). The
   only delivery path to Hermes is the blocking `pi-supervisor watch` client
   armed as a background run — no cronjob, no polling, no chat addressing.
7. **Shutdown stops the loops, not just the children.** `Supervisor.Shutdown`
   closes each active runner's stop channel (under `r.mu`, so a concurrent
   `Stop` cannot double-close) before signalling the client groups, so the
   daemon never spawns a fresh pi on its way out.

## Build / test / install

```bash
GOTOOLCHAIN=auto go build ./... && go vet ./...   # both must be clean
GOTOOLCHAIN=auto go test ./... -race              # 92 tests; -race is not optional
GOTOOLCHAIN=auto go build -o pi-supervisor ./cmd/pi-supervisor
./install/install.sh                              # build + unit + skill symlink + enable
systemctl --user status pi-supervisor             # active (running) = READY accepted
pi-supervisor status                              # ctl over the socket
```

Socket: `$XDG_RUNTIME_DIR/pi-supervisor.sock` (0600). CLI and socket use the
same JSON shapes (`internal/control.Request`/`Response`).

## Adding a job

Write `~/.pi/supervisor/jobs/<name>.json`:

```json
{
  "brief": "/tmp/pi_task_brief.md",
  "cont": "/tmp/pi_task_cont.txt",
  "final_report": "/tmp/pi_task_final_report.md",
  "marker": "ALL_TASK_DONE",
  "worktree": "/home/balor/workspace/eink/crosspoint-x-reader/_worktrees/<wt>",
  "session_name": "task",
  "session_path": "",           // left empty: round 1 LAUNCHes, then adopts
  "max_rounds": 6,
  "timeout_s": 1800,
  "skills": ["/home/balor/.hermes/skills/software-development/coding-philosophy"]
}
```

Then `pi-supervisor reload && pi-supervisor start <name>`. An existing session
JSONL can be pinned directly via `session_path` (session-reuse rule).

Credit: Antoine Aflalo (Belphemur), Hermes Agent.
