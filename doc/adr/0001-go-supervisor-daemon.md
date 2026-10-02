# ADR 0001 — pi-supervisor as a Go systemd daemon

Date: 2026-10-02
Status: Accepted
Credit: Antoine Aflalo (Belphemur), Hermes Agent

## Context

The bash `pi_supervisor.sh` resume loop kept dying mid-campaign. Three
failure classes, in order of pain:

1. **Agent-lifecycle reaping.** The supervisor ran inside the agent's
   background-tool process tree; context compression / session teardown reaped
   it even with `persist_on_release`. A supervisor that dies with the agent
   that launched it fails its only job.
2. **Instant-exit death spirals.** When pi's accumulated session hits the
   model's context wall (or a provider refuses), the client exits nonzero in
   seconds; a fixed `sleep 30` relaunch looped forever without diagnosis.
3. **No observability.** Status lived only in `/tmp` log files; the operator
   had to grep JSONLs to answer "is it alive, what round, why did it exit?"

## Decision

- **Go 1.27 single binary** (`cmd/pi-supervisor` + `internal/{notify,job,supervisor,control}`).
  Static, stdlib-only, no cgo — trivially installable and verifiable.
- **systemd `--user` service, `Type=notify`** with `Restart=on-failure` and
  `WatchdogSec=120`. systemd owns the daemon's lifetime, not the agent:
  crash ⇒ restart; hang ⇒ watchdog restart. Pure-Go `sd_notify` (READY /
  WATCHDOG / STOPPING) with no cgo dependency.
- **Unix-socket control API** (`$XDG_RUNTIME_DIR/pi-supervisor.sock`, 0600,
  one JSON line per connection) with a ctl subcommand in the same binary.
  Queryable while running — "can be directly questioned" — and steering goes
  through the same `.ctrl` file protocol `pi_rpc_client.py` already polls, so
  `pi_control.py` keeps working.
- **Job model on disk** (`~/.pi/supervisor/jobs/*.json` + persisted state in
  `~/.pi/supervisor/state/`): daemon restarts adopt jobs and resume by
  session JSONL path. Session paths are captured once after the first LAUNCH
  and never re-launched (session-fork guard, inherited from the bash script).

## Consequences

- The agent no longer owns the supervisor process; it only talks to the
  socket. Killing the session cannot kill the campaign.
- Client death is classified (natural 1800s cap vs instant implosion) and
  logged with a diagnostic tail; 3 consecutive instant exits stop the job in
  `fatal` instead of spinning.
- Backoff is adaptive: 15s after clean cap, 20s after long runs, 90s after
  instant implosions, 30s otherwise.
- The bash supervisor remains as historical reference; new work targets the
  daemon. Per-job file names (`/tmp/pi_<name>.*`) are unchanged for tooling
  compatibility.
