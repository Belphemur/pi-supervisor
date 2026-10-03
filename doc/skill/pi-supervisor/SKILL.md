---
name: pi-supervisor
version: 1.4.0
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
that runs the pi RPC resume loop for named jobs: spawns `pi --mode rpc` rounds
through its own Go client (no Python), resumes the same session JSONL across
rounds, classifies deaths, backs off adaptively, and answers queries over a
Unix socket. It replaces the bash `pi_supervisor.sh` for anything that must
outlive the launching agent. The CLI is cobra + viper (ADR-0008): every
subcommand has its own `--help`, tab-completion is installed for every shell
found on PATH, and `steer --interrupt` can drop the running turn.

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
pi-supervisor status <name>      # one job; ends with `pr <url>` when the
                                 # transcript linked a GitHub PR (ADR-0006)
pi-supervisor logs <name> 50     # tail the run log (one line per log line)
pi-supervisor steer <name> 'POLICY CHANGE FROM THE OWNER ...'
pi-supervisor steer <name> -i '...'  # INTERRUPT: SIGINT pi's group first, so the
                                 # running turn is dropped and this steer is
                                 # what pi does next (ADR-0007)
pi-supervisor steer <name> -n '...'  # don't wait for pi's ack
pi-supervisor stop <name>        # SIGTERM the client group; session kept

# Notifications: block until the supervisor sends an event (see below)
pi-supervisor watch <name>       # exit after the first event
pi-supervisor watch <name> -t    # exit only when the run is over (done/fatal/stopped)

# Shell completion (idempotent; detects bash/zsh/fish/powershell on PATH)
pi-supervisor completion install
pi-supervisor version                    # shows the build-time git commit (ADR-0009)
pi-supervisor restart <job> --fresh     # discard a poisoned session, start a clean one (ADR-0010)
pi-supervisor --help              # cobra: per-command help for every subcommand
```

## CLI — cobra + viper (ADR-0008)

The CLI is **cobra** (command tree, per-command help, real flag parsing,
completion generation) with **viper** for configuration. Nothing is positional
guesswork: every subcommand documents itself.

```bash
pi-supervisor --help              # the command tree
pi-supervisor <cmd> --help        # per-command: usage, examples, every flag
pi-supervisor steer --help        # shows -n/--no-wait AND -i/--interrupt
```

### Exit codes (a contract — scripts depend on these)

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | the daemon answered but refused (`ok:false`), or a malformed response |
| 2 | usage/validation: unknown subcommand, bad flag, missing argument — **and "daemon not reachable"** |

`watch` owns its own three exits (see Notifications below) because it streams
and distinguishes each terminal condition.

### Configuration (viper)

Precedence: **explicit flag > environment > config file > built-in default.**

| Setting | Env override | Default |
|---|---|---|
| socket | `PI_SUPERVISOR_SOCK`, `PI_SUPERVISOR_SOCKET` | `$XDG_RUNTIME_DIR/pi-supervisor.sock` |
| config file | `PI_SUPERVISOR_CONFIG` | `~/.config/pi-supervisor/config.yaml` |

A missing config file is not an error — every value has an env or default
fallback, so the daemon runs with no config at all.

### Shell completion

Completion is **part of the install**: `install/install.sh` runs
`pi-supervisor completion install`, which detects every supported shell on
PATH — bash, zsh, fish, powershell, with the login shell `$SHELL` always
first — generates that shell's script, writes it to the conventional
completion directory, and appends an **idempotent, marker-guarded** block to
the rc file where the shell does not auto-load. Re-running never duplicates
the block; remove it by deleting the `# >>> pi-supervisor completion >>>`
section.

```bash
pi-supervisor completion install    # detect + install for every shell found
pi-supervisor completion bash       # print one script to stdout (also zsh,
                                    #   fish, powershell) for manual sourcing
```

### Version

The CLI is versioned by the git commit it was built from (ADR-0009).
`install.sh` injects the short commit id at link time; a plain `go build`
without `-ldflags` reports `dev`.

```bash
pi-supervisor version    # "pi-supervisor f967040" on an installed binary
```

The version is also queryable programmatically via `internal/version` for
anything embedding the supervisor package.

Completion offers real job names (read from `~/.pi/supervisor/jobs/*.json`,
never from the daemon, so tab-completion can never block) and every flag, so
`pi-supervisor steer --inter<TAB>` completes to `--interrupt`.

`PI_SUPERVISOR_COMPLETION_HOME` retargets every completion/rc path, which is
how the tests exercise the install without touching the real home.

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
- **Completion:** final report exists AND the marker appears in an **assistant
  message in the session transcript** ⇒ job `done` (ADR-0011). The transcript
  is streamed live while the round runs, so the marker is detected mid-turn,
  and the latch is sticky for the job. It is deliberately NOT the run log:
  `round()` truncates `/tmp/pi_<job>_run.log` at the start of every round, so a
  marker from an earlier round is structurally invisible there — that bug made
  `mealime-roomux` burn 14 rounds on finished work (PR #43) and end `fatal`.
  Only assistant **text blocks** count: DCP compression summaries, the user
  brief, and `toolCall` arguments all quote the marker without the agent
  having finished.
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
- **Interrupting a steer:** `-i`/`--interrupt` first SIGINTs pi's process
  group (`-pid`), so the turn already in flight is asked to stop and this steer
  is what pi picks up next; the report then carries an extra `interrupt` line
  saying the signal was sent. `interrupted` means *sent*, never *obeyed* — the
  ack outcome stays the source of truth for delivery. If the SIGINT cannot be
  delivered, the steer fails loudly and writes NO frame rather than silently
  degrading to a queued one. A plain steer never signals. See ADR-0007.
- **Discarding a poisoned session:** `restart <job> --fresh` stops the round,
  moves the transcript to `<session-dir>/_archived-stale/<stem>_<ts>.jsonl`
  (move-only, bytes preserved), clears `session_path`, resets the round
  counter, and relaunches with brief+cont re-read from disk. `restart` without
  `--fresh` is a plain stop+start (same session). This is the supported
  replacement for the old manual `mv` + `rm state` + `start` dance — do not do
  that by hand, the daemon's sequence has no re-adoption window. See ADR-0010.
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
- **The pull request is in the payload.** The daemon scrapes
  `https://github.com/<owner>/<repo>/pull/<number>` out of the round's
  transcript as it tails it (ADR-0006): every event carries `pr_url`, the
  `round_done` output tail gains ` | pr <url>`, and the exit message prints
  `pull request: <url>`. It is a best-effort scrape, so `pr_url` is absent
  when the agent opened a PR without linking it — "" means "not linked", not
  "no PR".
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
| job ends `fatal` with "round cap reached without marker" but the work is visibly finished and a PR exists | the marker WAS emitted but the gate could not see it. Check `logs <job>` for `marker ... streamed from session transcript`; on a pre-ADR-0011 daemon this was the run-log truncation bug (fixed — upgrade). `status` now shows `marker_found` truthfully mid-round |
| `status` shows `marker_found: true` but the job is not `done` | the marker was seen but `final_report` does not exist yet — the second half of the gate. Write the report at the path in the job JSON |
| job state `fatal`, diag "3 consecutive instant exits" | session context wall — start a fresh session (new job or clear state) or trim the session |
| job keeps re-validating superseded work, or a round sits at 0 transcript bytes with pi alive | poisoned or wedged session — `pi-supervisor restart <job> --fresh` quarantines the transcript, resets the round counter, relaunches clean (ADR-0010). Do NOT hand-move the JSONL; the daemon sequence has no re-adoption window. |
|| `start` says "already done" | marker+report were reached; clear `~/.pi/supervisor/state/<name>.json` to rerun |
|| `start` after a stopped run silently resumes the OLD session (fresh-LAUNCH intent defeated) | **RESOLVED — use `pi-supervisor restart <job> --fresh`** (ADR-0010). It quarantines the transcript to `<dir>/_archived-stale/<stem>_<ts>.jsonl`, clears `session_path`, resets the round counter, and relaunches with brief+cont re-read from disk. (Pre-ADR-0010 you had to `mv` the JSONL out of the munged dir and clear the state file by hand; don't do that anymore — the daemon sequence has no re-adoption window.) |
|| `steer` reports `forwarded` but pi never acted on it | ack `outcome: forwarded` is written when the control frame lands on disk, NOT when pi reads it; the control file is truncated at each round start, so a steer delivered while pi is parked in a CI/review poll (`gh pr checks --watch`) is wiped unread and the `forwarded` ack becomes a false positive. Wait until `status` shows a live round NOT polling CI, then re-send; if the round is in its polling window, use `steer --interrupt` so the current turn is dropped and pi reads the frame on the next prompt. |
|| round is alive but `tool_use` stays 0 and `session_bytes` is frozen (pi pid alive, `Sl`, empty assistant turns) | empty-turn stall. **Now detected automatically** (ADR-0010): the first window emits an `empty_turn` event in `watch`; a second consecutive window escalates on the same abort+re-prompt path as `ci_stall`. Tune the window with `empty_turn_idle_s` in the job JSON (default 60s). |
| ctl: "daemon not reachable" | `systemctl --user status pi-supervisor`; journal for socket errors |
| `systemctl status` count is stale | the beat only rewrites STATUS when the count changes; a count that never moves means no round is ending |
| `steer -i` says `interrupt requested but cannot SIGINT` | the pi group was already gone (round ended between your `status` and the steer). Nothing was written — check `status`, then re-send; if the round is genuinely running this is a real failure, not a silent no-op |
| `steer` says `no live round` / `not confirmed` | no round was polling the ctrl file, or pi never acked within the bounded wait (~20s) — the frame was NOT delivered; check `status` and the run log, then re-send |
| rc=2 in the log | pi's stdout closed with no agent_end (crash mid-turn) — treated as a failure, not a clean cap |
| `status` shows no `pr <url>` but a PR is open on GitHub | the daemon scrapes the PR URL from the round's transcript as it tails it (ADR-0006); it only sees URLs the agent *linked* in its messages. A PR opened without the agent writing the `pull/<number>` link — e.g. a toolResult that truncated the URL, or a PR filed by CI/a hook — is not surfaced. An empty field means "not linked in the transcript", not "no PR exists"; link the PR in your next round to have it appear. |

## Pitfalls (from the bash era, still true)

- Never run the RPC client in a foreground tool call — the timeout kills it
  mid-turn. The daemon exists to make this moot.
- One steer = one coherent directive block; never re-send the whole brief.

### Live real-pi validation pitfalls

When writing a test that drives a real `pi` session through the real
supervisor path (the only way to prove the marker detector sees what pi
actually emits, not what the fixtures assume it emits):

- **`testEnv()` starves pi of credentials.** Redirecting `HOME` to a temp dir
  strips the real pi config + auth, so a real `pi` exits immediately without
  writing a transcript — a failure that is indistinguishable from the bug
  under test. Isolate via a private worktree + a namespaced job name; do NOT
  set a temp HOME, and do NOT use the seeded-resume path.
- **`t.Setenv` / `testEnv` is for unit fixtures, not integration.** The live
  test must call pi's real config dir and the real daemon socket; treat the
  real `$HOME` as part of the harness.
- **Prefer a true LAUNCH round over a seeded resume.** A seeded-resume path
  makes pi write the transcript into a session `FindSession` cannot discover,
  so the round completes with 0 bytes visible — again, structurally identical
  to the completion-gate bug. A real LAUNCH is the only shape that exercises
  the eager+`lazy` watcher resolution.
- **Steering blocks, it does not queue.** `steer` waits for the client's ack
  (bounded by the job's `timeout_s`, default ~20s) and exits non-zero unless
  pi took the frame; `-n` skips the wait and reports only `written`. A steer
  with no live round reports `no live round` — the ctrl file is truncated at
  every round start, so a queued frame would be wiped unread. Wait for
  `status` to show a round, then re-send.
- **`-i` interrupts; a plain steer queues.** `steer -i` SIGINTs pi's process
  group first (ADR-0007) so the current turn is dropped and the steer is what
  pi does next; the report line `interrupted` means the signal was *sent*, not
  that pi obeyed it. A plain steer never signals — it queues behind the turn
  already in flight. If the SIGINT cannot be delivered, `-i` fails loudly and
  writes **no** frame rather than degrading to a queued steer.
- Session JSONL is the memory: never delete it to "reset"; clear the job
  state instead.
