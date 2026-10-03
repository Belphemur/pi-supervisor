# AGENTS.md — pi-supervisor

Project: Go 1.27 daemon that supervises long-running [pi](https://github.com/withpi/pi)
RPC delegations (used by the CrossPoint/XPoint agent campaigns). It replaces the
bash `pi_supervisor.sh` resume loop with a systemd `--user` service that survives
agent lifecycle teardown, monitors session growth, and answers queries over a
Unix socket.

## Layout

| Path | Role |
|---|---|
| `cmd/pi-supervisor/main.go` | Cobra entry: `run` (daemon) + ctl subcommands (status/start/stop/steer/logs/reload/watch/version) + `completion install` |
| `cmd/pi-supervisor/completion.go` | Shell-completion install: detect bash/zsh/fish/powershell on PATH, generate each script, append an idempotent marker-guarded block to the rc file (ADR-0008) |
| `internal/notify/sd.go` | Pure-Go `sd_notify`: `READY=1`, `STOPPING=1`, and `Beat()` which pairs `WATCHDOG=1` with `STATUS=<n> parallel pi session(s) running`. No cgo. |
| `internal/job/job.go` | Job model (`~/.pi/supervisor/jobs/*.json`), state persistence, session discovery, run-log helpers, steer ack model (`job.Ack`, `job.AckRecord`, `job.SteerReport`) |
| `internal/client/client.go` | The pi RPC client, in Go: LF-JSON framing, streamed text, control-file steering (with per-frame acks), abort-drain handshake, timeout/abort/reap escalation. No Python. |
| `internal/version/version.go` | Build-time version string; injected via `-ldflags -X .../version.ver=<commit>` by install.sh, falls back to `dev` (ADR-0009) |
| `internal/supervisor/interrupt_test.go` | Real process-group SIGINT deliverability test |
| `internal/control/control_test.go` | fakeHandler 4-arg Steer + interrupt-flows-to-handler tests |
| `cmd/pi-supervisor/version_cli_test.go` | `pi-supervisor version` offline CLI test |
| `internal/supervisor/supervisor.go` | Round loops: drive `internal/client`, classify exits, adaptive backoff, marker gate, instant-exit strikes, never-fork guard, `Steer` (frame + ack wait, ADR-0005), `interruptPID` (SIGINT group, ADR-0007) |
| `internal/events/events.go` | Lifecycle events: append-only audit JSONL per job + in-process fan-out broker (buffered, never blocks the round loop) |
| `internal/job/transcript.go` | completion surface: `TranscriptContains`, `TranscriptWatcher` (ADR-0011) |
| `internal/stall/stall.go` | CI/review stall detector (ADR-0004): tails the session JSONL for CI-wait markers; stall = marker + idle window; drives the finish-the-report intervention at the cap. Same tail scrapes the round's GitHub PR URL (ADR-0006) |
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
   tooling (`pi_control.py`, orchestrator greps) depends on them. The ack log
   `/tmp/pi_<name>_ack.jsonl` (ADR-0005) is additive and advisory.
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
8. **Steer never claims success it cannot prove (ADR-0005).** Prose is wrapped
   into a `prompt` frame (the client's wire contract stays strict JSON-LF);
   each frame carries an id; the client appends an ack record per frame it
   acts on and `steer` waits (bounded by the poll interval, holding no lock)
   for the terminal one. `no live round` and `not confirmed` are failures with
   a non-zero exit, never a quiet exit 0. The ctrl file is truncated at every
   round start **and the reader starts at offset 0** — those two are one
   mechanism; don't "fix" one without the other.
9. **`pr_url` is a best-effort transcript scrape (ADR-0006).** No GitHub auth,
   no `gh`, no network: the PR URL comes from the session JSONL the daemon
   already tails for CI stalls (`stall.prRe`, digits required so a truncated
   `…/pull/` never matches). It is stored on `job.State` and surfaced on every
   event, so `status <job>` ends with `pr <url>` and `watch` prints it. "" means
   "the agent never linked a PR", never "no PR exists" — don't make it an error
   path, and don't add a second tailer for it.
10. **`steer --interrupt` is opt-in and fail-loud (ADR-0007).** `-i`/`--interrupt`
   SIGINTs pi's process group (`-pid`) BEFORE writing the frame, so the running
   turn is asked to stop and the steer is what pi picks up next. A failed signal
   is an error and writes NO frame — never silently degrade to a queued steer.
   The report marks the signal as sent (`interrupted`), never as obeyed; a plain
   steer never signals.
11. **Exit codes are a CLI contract (ADR-0008).** 2 = usage/validation (and
   "no daemon on the socket"), 1 = daemon refused / bad response, 0 = success.
   The CLI is cobra + viper; viper binds config as flag > `PI_SUPERVISOR_*` env >
   `~/.config/pi-supervisor/config.yaml` > default. `completion install` only
   ever APPENDS an idempotent marker-guarded block to an rc file; never clobber.
12. **`restart --fresh` is the only sanctioned way to drop a session (ADR-0010).**
   Stop → quarantine (`_archived-stale/<stem>_<ts>.jsonl`, move-only, bytes
   preserved) → clear `session_path` + reset `Round` → relaunch LAUNCH with
   brief+cont read from disk. The quarantine TARGET being a subdirectory is what
   makes re-discovery structurally unable to re-adopt it (`FindSession` skips
   dirs) — don't "simplify" `job.Quarantine` to write in place. `restart` without
   `--fresh` is a plain stop+start. Never hand-move the JSONL or delete the state
   file to force a fresh start.
13. **Empty-turn stall needs BOTH conditions (ADR-0010).** `stall.EmptyTurn`
   fires only when the transcript is frozen AND zero `tool_use` markers were
   seen in the window — growth without a tool call is the model streaming prose,
   which is normal work. One `empty_turn` event, then escalation on the
   `ci_stall` abort+prompt path. Window = `empty_turn_idle_s` (default 60s).
15. **Completion is detected in the SESSION TRANSCRIPT, streamed (ADR-0011).**
   The marker gate reads assistant TEXT BLOCKS of the transcript JSONL, latched
   live by `watchMarker` while the round runs and sticky thereafter. Never read
   the marker from the run log: `round()` truncates `/tmp/pi_<job>_run.log`
   every round, so a marker from an earlier round is invisible and a finished
   job livelocks to MaxRounds (mealime-roomux, PR #43, 14 wasted rounds). Two
   traps when scanning: DCP `custom`/dcp-state summaries QUOTE the marker, and
   so do the user brief and `toolCall` arguments — only `type:"message"` +
   `role:"assistant"` + `text` blocks count. `done` still requires BOTH the
   marker AND `final_report`, and an empty marker never matches
   (`strings.Contains(s, "")` is true for any non-empty s).
14. **A steer `consumed` ack is deliberately absent (ADR-0010).** The ctrl file
   is read by the supervisor's own client goroutine, not pi, so no observable
   event distinguishes "pi buffered it" from "pi is parked in a CI poll". A
   `consumed` ack derived from the ctrl reader's offset would be true by
   construction — exactly the false-delivery signal ADR-0005 exists to kill. Use
   `steer --interrupt` instead. Don't add one.

## Build / test / install

```bash
GOTOOLCHAIN=auto go build ./... && go vet ./...   # both must be clean
gofmt -l .                                         # must print nothing
GOTOOLCHAIN=auto go test ./... -race              # -race is not optional
/home/balor/go/bin/golangci-lint run ./...        # must exit 0 (v2.14.0)
pi-supervisor completion install               # detect+install shell completion (idempotent)
GOTOOLCHAIN=auto go build -ldflags="-X pi-supervisor/internal/version.ver=$(git rev-parse --short HEAD)" -o pi-supervisor ./cmd/pi-supervisor
pi-supervisor version                              # prints the commit id (dev if unlinked)
./install/install.sh                              # build + unit + skill symlink + enable
systemctl --user status pi-supervisor             # active (running) = READY accepted
pi-supervisor status                              # ctl over the socket
```

### golangci-lint gate

`.golangci.yml` (schema `version: "2"`) enables errcheck, govet,
staticcheck (incl. gosimple), ineffassign, unused, bodyclose, errorlint,
copyloopvar, misspell, unconvert and a curated low-noise revive rule set.
No whitespace/lll/godot/godox — they are stylistic churn, not defects.

Toolchain caveats that cost real time:

- The binary is **not on PATH** for non-login shells. Always invoke it as
  `/home/balor/go/bin/golangci-lint run ./...` (absolute path).
- It must be **built against go1.27.0** (this module declares `go 1.27`).
  Rebuild with:
  ```bash
  cd /tmp && GOTOOLCHAIN=go1.27.0 GOBIN=/home/balor/go/bin \
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
  ```
  Plain `GOTOOLCHAIN=auto` builds it with go1.26 and it then REFUSES to run
  ("language version used to build golangci-lint is lower than the targeted
  Go version").
- Do **not** downgrade to v2.12.2: its bundled staticcheck (honnef.co/go/tools
  v0.7.0) panics on go1.27 IR.
- Suppression policy: fix in code, not with `//nolint`. An explicitly
  ignored error (`_ = f.Close()`) is acceptable only where the call cannot
  fail meaningfully AND carries a comment saying why.

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
