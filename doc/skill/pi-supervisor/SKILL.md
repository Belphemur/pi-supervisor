---
name: pi-supervisor
version: 1.5.0
author: Antoine Aflalo (Belphemur), Hermes Agent
license: MIT
platforms: [linux]
description: "Supervise long-running pi delegations via a systemd daemon."
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
pi-supervisor status <name>      # one job; stdout is exactly one JSON
                                 # document (jq-parseable) carrying pr_url
                                 # when the transcript linked a PR (ADR-0006)
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
|| job ends `fatal` with "round cap reached without marker" but the code is committed + pushed and a PR exists | **The marker WAS emitted but the gate could not latch it.** Verify before concluding anything is wrong: `grep -c '<MARKER>' <session_path>` and, decisively, `grep '<MARKER>' <session_path> | grep -c '"role":"assistant"'` — a non-zero count in an ASSISTANT message proves the agent finished (per ADR-0011 only assistant text counts). Then check whether the FINAL REPORT exists; pi often finishes the work, emits the marker, and dies before writing the report. In that case the supervisor cannot be trusted to have recorded the outcome — re-derive it yourself from the commits (`git log <base>..HEAD`, `git diff --stat`) and RE-RUN the gates rather than trusting the run log, then write the missing report. Re-running the gates is not optional: a run log claiming "CI 8/8 green, all shipped" is a self-report. |
| `start` says "already done" | marker+report were reached; clear `~/.pi/supervisor/state/<name>.json` to rerun |
|| `start` after a stopped run silently resumes the OLD session (fresh-LAUNCH intent defeated) | **RESOLVED — use `pi-supervisor restart <job> --fresh`** (ADR-0010). It quarantines the transcript to `<dir>/_archived-stale/<stem>_<ts>.jsonl`, clears `session_path`, resets the round counter, and relaunches with brief+cont re-read from disk. (Pre-ADR-0010 you had to `mv` the JSONL out of the munged dir and clear the state file by hand; don't do that anymore — the daemon sequence has no re-adoption window.) |
|| `steer` reports `forwarded` but pi never acted on it | ack `outcome: forwarded` is written when the control frame lands on disk, NOT when pi reads it; the control file is truncated at each round start, so a steer delivered while pi is parked in a CI/review poll (`gh pr checks --watch`) is wiped unread and the `forwarded` ack becomes a false positive. Wait until `status` shows a live round NOT polling CI, then re-send; if the round is in its polling window, use `steer --interrupt` so the current turn is dropped and pi reads the frame on the next prompt. |
|| round is alive but `tool_use` stays 0 and `session_bytes` is frozen (pi pid alive, `Sl`, empty assistant turns) | empty-turn stall. **Now detected automatically** (ADR-0010): the first window emits an `empty_turn` event in `watch`; a second consecutive window escalates on the same abort+re-prompt path as `ci_stall`. Tune the window with `empty_turn_idle_s` in the job JSON (default 60s). |
| ctl: "daemon not reachable" | `systemctl --user status pi-supervisor`; journal for socket errors |
| `systemctl status` count is stale | the beat only rewrites STATUS when the count changes; a count that never moves means no round is ending |
| `steer -i` says `interrupt requested but cannot SIGINT` | the pi group was already gone (round ended between your `status` and the steer). Nothing was written — check `status`, then re-send; if the round is genuinely running this is a real failure, not a silent no-op |
| `steer` says `no live round` / `not confirmed` | no round was polling the ctrl file, or pi never acked within the bounded wait (~20s) — the frame was NOT delivered; check `status` and the run log, then re-send |
| rc=2 in the log | pi's stdout closed with no agent_end (crash mid-turn) — treated as a failure, not a clean cap |
| `status` shows an empty `pr_url` but a PR is open on GitHub | the daemon scrapes the PR URL from the round's transcript as it tails it (ADR-0006); it only sees URLs the agent *linked* in its messages. A PR opened without the agent writing the `pull/<number>` link — e.g. a toolResult that truncated the URL, or a PR filed by CI/a hook — is not surfaced. An empty field means "not linked in the transcript", not "no PR exists"; link the PR in your next round to have it appear. |
| `pr_url` empty although `pull/N` IS in the transcript, and auto-review skipped with "no GitHub PR was linked" | the scrape rides on the CI/empty-turn STALL watchers, which are only armed once a transcript path exists (`sess != ""`). On a fresh LAUNCH round the watcher has no path yet, so a URL written during THAT round is missed. Link the PR in a LATER round, or pass `--pr <N>` explicitly rather than relying on `--auto`. |
| `empty_turn` fired while the agent was demonstrably working | the detector reads TRANSCRIPT GROWTH, which stops while pi blocks on a child process (`go test -race`, a CI poll, a detached e2e run it launched). It cannot distinguish "model streaming prose" from "agent waiting on a subprocess". Raise the window per job — `empty_turn_idle_s`, default 60s, is tight for build/test work; 300s suits a job that waits on children. Known design limit, not a misfire. |
| a re-check says "no review baseline recorded" | correct, not an error: the job never ran a review campaign, so there is nothing to compare against. A baseline is written when a campaign ENDS. |
| `reload` ignores an edit to `state/<name>.json` | expected: `LoadJobs` refreshes the job DEFINITION for already-loaded jobs and reads persisted state only when CREATING an entry. An out-of-band state edit therefore has no effect, and the daemon overwrites the file from memory anyway. Do not seed state to force a code path. |
| `start`/`restart` refuses with "already done" | correct product behaviour — the completion gate was reached. To re-run, clear `~/.pi/supervisor/state/<name>.json` deliberately, or use a new job name; do not route around it by editing state mid-flight. |

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
  set a temp HOME, and do NOT use the seeded-resume path. The session's
  two false failures were *both* `testEnv()`/`HOME`-driven and *both* timed
  out at the same line — do not treat a timeout as a code bug until you have
  proven the transcript was readable at all.
- **Distinguish failure shape by transcript size.** A credential-starved live
  test leaves a ~0-byte transcript (pi exited before writing) — verify
  `session_bytes` before assuming the gate missed a marker. A discovery
  failure (seeded resume into a path `FindSession` cannot resolve) leaves
  `session_path` pointing somewhere the watcher never tails — the run log
  stays empty while pi is genuinely alive and silent. These two shapes are
  structurally identical at the 90s timeout, but their fixes are different:
  the first is a harness env fix, the second is a round-shape fix (use LAUNCH).
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

### 7. Review campaigns (ADR-0012)

A review campaign is a job whose fix-rounds the daemon orchestrates rather
than the agent: the daemon owns the outer loop (poll GitHub -> decide ->
enforce budget), pi runs each round, and the two are joined by the completion
gate (ADR-0011) — a round ends at its marker, and the gate's `pr_url` is what
arms the review phase. **Every command here is consumed by another LLM, not a
human**: nothing prompts, nothing pages, and every failure is a non-zero exit
plus a machine-readable `reason` from a closed enum.

- **Surface.** `pi-supervisor review <job> --pr <N> [--rounds N=5] [--type acceptance|rebuttal] [--json]` / `review <job> --auto` / `ack --event <ack_id>`. `--pr` and `--auto` are mutually exclusive. `--rounds` is the campaign's `MaxRounds` — **default 5** (a review round is hours-long; an operator count beats a heuristic); `--rounds 0` auto-derives `ceil(open / review.per_round)` (`per_round` 12) capped by `review.max_rounds` (12). `--type` is the campaign's round character, uniform across its rounds and recorded per round so `watch`/`status` surface `#acceptance` vs `#rebuttal`; mixed campaigns are two campaigns.
- **One instruction source, not two.** `--skill` defaults to
  **`pi_supervisor_review`**, which carries the triage vocabulary (fix /
  explain-non-issue / defer-out-of-scope) *and* the shim contract. It is NOT
  `answer-code-review`: that skill's body is a set of instructions to run
  `reply_review.py`, so injecting it alongside a contract that forbids the
  agent from touching `reply_review.py` puts two contradictory directives in
  one prompt, and the model follows whichever it reads last. `answer-code-review`
  stays the default for **non**-review jobs and remains available here via an
  explicit `--skill` for prose reference. Its proven GraphQL query shapes are
  inherited by the daemon, not re-derived.
- **The verb set is closed at six**, and it is the only way a review round
  reaches GitHub: `list_threads` | `thread_detail` | `post_replies` |
  `resolve_thread` | `bulk_resolve` | `check_ci`. The shim
  (`_pi-supervisor-review`, installed on PATH by `install.sh` as a symlink to
  the canonical copy in `doc/skill/pi_supervisor_review/`) is a CLI speaking the
  daemon's JSON control protocol directly over the unix socket — not an MCP
  plugin process. New endpoint, additive: `POST /review/action`.
- **Call order is load-bearing: `post_replies` → `resolve_thread`, always.** The
  model's instinct is to close a thread once it judges the work finished; here
  that is the one thing that fails. `resolve_thread` is refused with
  `not-answered-this-round` unless a `post_replies` in **this** round already
  touched that thread — reply bodies are the record of *why* a finding was
  fixed, rebutted, or deferred, and a bare close discards that. `post_replies`
  takes a batch (`(thread_id, type{acceptance|rebuttal}, body)` tuples) so one
  model turn answers every thread it triaged, each with its own body and
  classification. The daemon stamps `job`/`round` itself — never from the
  request payload — so the guard cannot be self-certified; a mismatch is refused
  (`round-mismatch`), not silently corrected.
- **Errors are a closed enum the caller branches on**, never prose: `no-live-round`,
  `round-mismatch`, `not-answered-this-round`, `unknown-thread` (exit 2 — fix the
  call), `auth-unavailable`, `rate-limited`, `github-error` (exit 1). `--json` is
  on every verb; ADR-0008's exit contract (2 usage, 1 refused, 0 success) is unchanged.
- **Two control planes, cleanly split.** Daemon = poll GitHub (read-only) -> decide "one more pi round?" -> enforce `MaxRounds`. Pi = one round's fix + triage. The daemon never authors replies or decides to resolve; it only *serves* reads and *records* writes the shim asks for. It refuses any review request when no `review <job>` round is live, so the shim is only valid inside the round it was armed for.
- **Thread ids are `PRRT_…` GraphQL node ids, never numeric.** This is the
  #1 way to waste a review turn: the numeric `id` from
  `GET /pulls/<n>/comments` is a *comment* node, and both mutations reject it
  with *"Could not resolve to a node with the global id"*. The daemon is the
  only producer of thread ids and always hands out `PRRT_…`; the shim never
  derives one from a URL or a REST payload. Two more inherited rules, both
  silent when broken: `author` lives on the **comment** node (not the thread —
  selecting it on the thread fails schema validation on every poll and reads as
  "no threads yet"), and `reviewThreads` pagination must run to **exhaustion**
  (it returns oldest-first, so page 1 is mostly resolved history and `--open`
  reports `0` while dozens are open — hit on PR #184, 67 threads). A response
  `cursor:null` means exhausted, not "first page".
- **Reads: thread reads are GraphQL, PR/CI metadata is REST.** `list_threads` /
  `thread_detail` use the GraphQL `reviewThreads` connection — the only surface
  with thread identity, `isResolved`, and comment authors; REST
  pulls-comments has none of the three. `head_sha`/PR state/CI verdicts use
  `go-github` REST. `check_ci` reads the Actions run's **`/jobs`** endpoint for
  the current head sha, never a `gh pr checks` rollup (that rollup races right
  after a push and transiently reads green with work still queued);
  `success`/`neutral`/`skipped` pass, `pending` does not.
- **Mutation field asymmetry.** `addPullRequestReviewThreadReply` takes
  `pullRequestReviewThreadId`; `resolveReviewThread` takes `threadId`. Same
  `PRRT_…` value, different field name — getting it backwards fails silently.
- **Auth (SDK, not shell-out).** `go-github/v90` (REST) + `shurcooL/githubv4` (GraphQL), **one shared token source** — `githubv4` wraps a plain `http.Client`, so both clients share one authenticated transport:
  - GitHub App (preferred): `GITHUB_APP_ID` + `GITHUB_APP_PRIVATE_KEY` (inline PEM or path) -> `golang-jwt` signs a JWT -> `go-github`'s `InstallationTokenSource` fetches + caches + auto-refreshes a 1h installation token. No PAT on disk.
  - fallback: if the App env is unset, the daemon runs `gh auth token` (the *only* `gh` call in its lifecycle — token acquisition, never an API call) and passes that token to the same `go-github`/`go-githubv4` clients. If `gh auth token` fails, `Start` refuses (exit 1) with `GitHub auth unavailable: run 'gh auth login' or set GITHUB_APP_ID`.
  - No `gh api` anywhere. No second credential surface. The daemon refuses a `/review/action` request when no `review <job>` round is live — the shim is only valid inside the round it was armed for.
- **Auto-trigger on marker + open PR.** When a marked round (marker **and**
  final report) links an OPEN PR (`pr_url` from §4 scrape) and the job def
  carries `auto_review`: the gate closes the build job first, then the job
  transitions `done → reviewing` and the daemon (1) posts the fixed
  `@coderabbitai review` comment, (2) waits the warmup
  (`review.coderabbit_warmup`, default 5m — a fresh PR has zero threads until
  CodeRabbit finishes its pass), (3) re-checks paginated: > 0 threads -> run
  the campaign (default 5 rounds); 0 threads -> emit `review_skipped` with
  `reason:"no open threads after CodeRabbit warmup"` and leave the job `done`.
  The `done → reviewing` hop is load-bearing: the gate clears `active` and
  `Start` refuses a `done` job, so there is no live round to fire from — the
  trigger runs in the gate's tail, and review rounds **resume** the same
  session under their own round counter. `auto_review` is consumed on fire (the
  marker latch is sticky, so an unguarded re-arm would loop forever); re-arm
  explicitly with `review <job> --auto`. The PR number is parsed from the
  `pull/<N>` in the already-scraped `pr_url`; no second scrape.
- **Round economics: no-push is free.** Only pi-execution rounds consume
  `MaxRounds`. A round that replied to threads but did not push a new commit
  leaves the head SHA unchanged; the next round re-handles the stale threads
  without burning budget — including a rebuttal round that pushes back with
  evidence but no fix commit (the answer itself is the round's work; only the
  no-push-answer case is free).
- **`watch` is the review dashboard, and its terminal events matter.** Arm it
  after `review`/`start`. `review_round_done` carries round N/M, round type,
  open-thread count, and the CI verdict — naming the failing **required** checks,
  never just "fail". Terminal events are `review_done` (clean) and
  `review_exhausted` (budget spent); **`reviewing` is deliberately NOT terminal**,
  because the build job is done while the campaign still owns the loop, so a
  watch that closed there would stop listening mid-campaign. The gate emits
  `reviewing` (never `done`) on the handoff. Every review event prints an
  LLM-facing footer: `reviewing` says the job is NOT over,
  `bulk_resolve_requested` says nothing was closed yet and names the ack
  command, `bulk_resolve_expired` says the threads are still open, and
  `review_exhausted` gives the re-arm command with more budget.
- **Only REQUIRED checks block the campaign.** `check_ci` reads the Actions
  run's `/jobs` for the current head sha (never a `gh pr checks` rollup, which
  races state transitions right after a push) and asks GraphQL
  `isRequired(pullRequestId:)` which of them the PR's protection rules demand —
  the `/jobs` endpoint has no such flag, and guessing from a job name would ship
  a red required check. A failing **optional** check is reported in
  `non_blocking` and in the round message but never blocks; `neutral`/`skipped`
  count as passing. If GitHub will not report required-ness, the rollup treats
  every check as required and flags `required_unknown` — failing closed, since a
  spurious "not done" costs rounds while a missed red check costs a merge.
- **The campaign owns its round budget.** While a campaign is live the loop reads
  the campaign's `MaxRounds`, not the build job's, so a 5-round campaign cannot
  run to the job's default 200. Exhausting it emits `review_exhausted` — a
  distinct failure from a build job's "round cap reached without marker",
  because there is no marker in a review round.
- **Pre-merge is a gate; bulk-close is ack-gated, never human-gated.** The
  campaign *reports* readiness — the owner merges once threads read zero, and
  the daemon never merges. Bulk close is a `bulk_resolve` action the LLM may
  invoke: it posts a PR comment (thread IDs + reason), emits
  `bulk_resolve_requested` with an `ack_id`, and applies the mutations **only**
  after a peer ACK (`pi-supervisor ack --event <ack_id>`). Because no human is
  ever present, an unacked request **expires** after `review.ack_timeout`
  (default `30m`) and applies nothing, emitting `bulk_resolve_expired` — the
  threads stay open, which is the safe default. So out-of-scope threads can be
  closed, but never silently and never into a deadlock.
