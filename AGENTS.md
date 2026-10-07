# AGENTS.md — pi-supervisor

Project: Go 1.27 daemon that supervises long-running [pi](https://github.com/withpi/pi)
RPC delegations (used by the CrossPoint/XPoint agent campaigns). It replaces the
bash `pi_supervisor.sh` resume loop with a systemd `--user` service that survives
agent lifecycle teardown, monitors session growth, and answers queries over a
Unix socket.

Architecture decisions: [doc/adr/index.md](doc/adr/index.md) — the table of
contents for every ADR. When adding or revising an ADR, update that index in
the same commit; this file links to it instead of duplicating it.

## Layout

| Path | Role |
|---|---|
| `cmd/pi-supervisor/main.go` | Cobra entry: `run` (daemon) + ctl subcommands (status/start/stop/steer/logs/reload/watch/version) + `completion install` |
| `cmd/pi-supervisor/completion.go` | Shell-completion install: detect bash/zsh/fish/powershell on PATH, generate each script, append an idempotent marker-guarded block to the rc file (ADR-0008) |
| `internal/notify/sd.go` | Pure-Go `sd_notify`: `READY=1`, `STOPPING=1`, and `Beat()`/`BeatStatus()` which pair `WATCHDOG=1` with `STATUS=<n> parallel pi session(s) running` plus an optional note (the fatal-job list). No cgo. |
| `internal/job/job.go` | Job model (`~/.pi/supervisor/jobs/*.json`), state persistence, session discovery, run-log helpers, steer ack model (`job.Ack`, `job.AckRecord`, `job.SteerReport`) |
| `internal/client/client.go` | The pi RPC client, in Go: LF-JSON framing, streamed text, control-file steering (with per-frame acks), abort-drain handshake, timeout/abort/reap escalation. No Python. |
| `internal/version/version.go` | Build-time version string; injected via `-ldflags -X .../version.ver=<commit>` by install.sh, falls back to `dev` (ADR-0009) |
| `internal/supervisor/interrupt_test.go` | Real process-group SIGINT deliverability test |
| `internal/control/control_test.go` | fakeHandler 4-arg Steer + interrupt-flows-to-handler tests |
| `cmd/pi-supervisor/version_cli_test.go` | `pi-supervisor version` offline CLI test |
| `internal/supervisor/supervisor.go` | Round loops: drive `internal/client`, classify exits, adaptive backoff, marker gate, instant-exit strikes, never-fork guard, `Steer` (frame + ack wait, ADR-0005), `interruptPID` (SIGINT group, ADR-0007) |
| `internal/journal/journal.go` | Structured stdout log for `journalctl`: one `<ts> <LEVEL> <subsystem> event=<name> key=value…` line per transition, custom `slog.Handler` (stdlib only, no third-party logger), shared write mutex so concurrent job loops never interleave a line |
| `internal/fault/fault.go` | The ONE machine-readable refusal vocabulary for both the journal's `reason=` and the socket's `Response.Reason` (`fault.Kind`, its `All()` inventory, the `exitCodes` exit table). `internal/review.Reason` is an alias of it, so the two closed enums can never drift into two spellings of one condition (ADR-0013 follow-up 1b). Pure-Go, imports nothing. |
| `internal/events/events.go` | Lifecycle events: append-only audit JSONL per job + in-process fan-out broker (buffered, never blocks the round loop) |
| `internal/job/transcript.go` | completion surface: `TranscriptContains`, `TranscriptWatcher` (ADR-0011) |
| `internal/stall/stall.go` | CI/review stall detector (ADR-0004): tails the session JSONL for CI-wait markers; stall = marker + idle window; drives the finish-the-report intervention at the cap. Same tail scrapes the round's GitHub PR URL (ADR-0006) |
| `internal/review/` | GitHub client for ADR-0012: GraphQL for review threads (the only surface with thread identity/`isResolved`/author), REST for PR + CI metadata, one shared token source (ghinstallation for the App, `gh auth token` fallback). Holds the closed `Reason` enum that maps to exit codes |
| `internal/supervisor/review_*.go` | The review surface: campaign state + answer-before-resolve guard (`review_campaign.go`), the six verbs (`review_action.go`), campaign arming + client cache (`review_start.go`), the `done → reviewing` auto-trigger (`review_auto.go`), ack expiry (`ExpireAcks`) |
| `doc/skill/pi_supervisor_review/` | The injected review-round skill + the `_pi-supervisor-review` shim pi calls (ADR-0012) |
| `internal/control/control.go` | Unix-socket server: one-shot request/response + streaming `watch` (pushes events, closes on terminal) |
| `install/install.sh` | Build + systemd unit + Hermes skill symlink + verification |
| `install/pi-supervisor.service` | `Type=notify` user unit (`WatchdogSec=120`) |
| `doc/adr/` | Architecture decision records |
| `doc/skill/pi-supervisor/` | Hermes skill (canonical copy; skills dir symlinks here) |

## Invariants (do not break)

1. **Never re-LAUNCH a session.** After the first round captures the session
   JSONL path, every later round RESUMEs it (`--session`). A second LAUNCH
   forks the session and splits the work. `loop()` enforces this via the
   `pinned` check; keep it. The ONE sanctioned exception is at campaign ARM
   time (ADR-0018): arming a review campaign with `review_brief` clears the
   pin once, so the campaign's round 1 LAUNCHes a FRESH session seeded with
   the review brief — and that session stays pinned for every campaign
   round. Resuming the finished build session instead made campaign rounds
   re-verify old work and exit (mealime-search3, flambette#58).
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
   The arm is ALWAYS `watch -t` (+ persist_on_release): a plain watch is a
   one-shot probe that exits on the first event and leaves the terminal one
   unwitnessed.
   A watch whose stream was LIVE (watch_ack received) reconnects on daemon
   restart with bounded backoff and re-derives the missed terminal state via
   the precheck (ADR-0015); a connection that never answered exits 1 at once.
   The daemon's OWN shutdown records `stop_source: daemon` (ADR-0017), and a
   re-arming watch RESUMES such a job (pinned session, round intact) — an
   operator stop stays stopped. A campaign only ends when someone asks.
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
   event, so `status <job>` carries it as the JSON `pr_url` field and `watch`
   prints the URL in its footer. The status command's stdout is exactly ONE
   JSON document — jq-parseable — and no trailing human line may be appended.
   "" means
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
16. **Review thread ids are `PRRT_` GraphQL node ids, end to end (ADR-0012).**
    A numeric id from `GET /pulls/<n>/comments` is a *comment* node and both
    mutations reject it. `internal/review.ValidThreadID` is the only gate, and
    the daemon is the only producer of ids on this surface — never add a path
    that accepts a comment id, and never let the shim derive one.
17. **The daemon stamps `job`/`round` for every review call.** A client-supplied
    round is CHECKED, never used, and a mismatch is refused (`round-mismatch`).
    If the shim could name its own round the answer-before-resolve guard would be
    self-certifying — the ADR-0005 false-delivery class. Authorization
    (is a round live?) is checked BEFORE input validation or any GitHub call, so
    an out-of-round caller learns nothing about id validity or PR existence.
18. **Answer-before-resolve is enforced per ROUND, not per campaign.**
    `answeredInRound(round, id)` is the guard; a reply from round N-1 must not
    authorize a close in round N. `markAnswered` runs only AFTER every reply in
    the batch actually landed, so a failed batch never authorizes a close.
19. **Bulk close is ack-gated and EXPIRES (ADR-0012 §2.3).** `bulk_resolve` posts
    the audit comment and returns an `ack_id`; it applies nothing until
    `pi-supervisor ack`. Unacked requests are dropped by `ExpireAcks` after
    `review.ack_timeout` and the threads stay open — fail closed, never a
    deadlock, never silent. There is no human ACK: every consumer is an LLM, so
    an interactive gate would block forever.
20. **Review errors are a closed `Reason` enum, never prose.** `review.Reason`
    is a type ALIAS of `fault.Kind` (ADR-0013 follow-up 1b): one vocabulary for
    the `Response.Reason` field, one spelling per condition, `ExitCode()` living
    on `fault.Kind` as the `exitCodes` data table. Adding a Kind without an
    exit-code entry, or re-adding a hyphen/underscore twin of an existing value,
    fails a test (`TestEveryKindHasAnExitCode`, `TestNoDuplicateSpellings`) —
    keep it that way. `ExitCode()` on the reason is what maps to ADR-0008's
    contract (2 = change the call, 1 = retry). Adding a failure path that returns
    a bare `fmt.Errorf` breaks the shim's ability to branch — including the
    control-socket bad-JSON path, which must set `Reason: fault.KindUsage`.
21. **Only REQUIRED CI checks block a review campaign.** `check_ci` asks GraphQL
    `isRequired(pullRequestId:)` because the Actions `/jobs` endpoint carries no
    required-ness flag. A failing OPTIONAL check is reported in `non_blocking`
    and must NOT block — one flaky non-gating check would otherwise burn the
    whole round budget and end `review_exhausted` with real findings untouched.
    When required-ness is undeterminable, fail CLOSED (treat all as required) and
    set `required_unknown`; never guess permissively.
22. **`reviewing` is not a terminal event; `review_done`/`review_exhausted` are.**
    The gate closes the build job and enters the review phase in ONE step, so
    emitting terminal `done` there would make `watch` print "THE RUN IS OVER",
    exit 0, and stop listening mid-campaign. `internal/events.Terminal` and the
    `watchCtl` exit list must stay in sync with that decision.
23. **A live campaign owns its round budget.** `loop` reads the campaign's
    `MaxRounds` while `state == "reviewing"`, not the build job's — otherwise a
    5-round campaign runs to the job's default 200. A MANUAL campaign reaches
    that state the same way the auto trigger does: `Start` permits a done job
    when a campaign is armed and enters `reviewing` (ADR-0016 — no state-file
    dance), and the marker gate is INERT while `reviewing`, since the sticky
    latch of the PREVIOUS campaign would otherwise close a resumed session
    done at round 1. A campaign is spent at its terminal moments (`finish()`
    and `reviewGate`'s clean close clear it); operator stops keep it armed.
24. **Required-ness comes from branch protection, not GraphQL `isRequired`.**
    A live test showed `checkRun.isRequired(pullRequestId:)` returns empty check
    runs for a fork PR, so it silently degraded to fail-closed — every check
    blocking, which defeats the optional-check split. Use
    `Repositories.GetBranchProtection(base)`. Also: an UNPROTECTED branch
    (`no_protection`) requires nothing and blocks nothing — that is the opposite
    of `required_unknown`, and conflating them deadlocks campaigns. go-github
    reports that 404 as a plain `errors.New("branch is not protected")`, not an
    `*ErrorResponse`, so `isNotProtected` matches the message too.
25. **`thread_detail` needs `... on PullRequestReviewThread` wrapping the whole
    selection.** `isResolved` is not on the `Node` interface; the fragment on the
    inner field is also rejected. Both misplacements were found by the live test.
26. **There is a live GitHub test, read-only.** `internal/review/live_gh_test.go`
    runs against a real PR when `PI_SUPERVISOR_LIVE_GH=1` (+ `_LIVE_REPO`,
    `_LIVE_PR`). It only READS — never post/reply/resolve against a real PR.
    Run it after touching the GraphQL queries or the CI rollup; unit fixtures
    cannot catch a schema or shape error.
27. **go-github v90 renamed its services.** `Pulls` → `PullRequests`,
    `Apps.FindRepositoryInstallation` → `GetRepositoryInstallation`, and
    `AppsTransport`/`InstallationTokenSource` were REMOVED — App auth now goes
    through `github.com/bradleyfalzon/ghinstallation/v2` (`NewAppsTransport` then
    `NewFromAppsTransport`), which is what go-github's own docs recommend. Do not
    reintroduce the v69-era App helpers; there is no hand-rolled JWT or refresh
    loop in this tree.
28. **The journal is ADDITIVE and one-line-per-transition.** `internal/journal`
    writes `<ts> <LEVEL> <subsystem> event=<name> key=value…` for
    `journalctl --user -u pi-supervisor -o cat`; it never replaces the
    `/tmp/pi_*` files (invariant 5 still holds), it never logs streamed text,
    payloads or credentials, and every handler shares ONE write mutex so
    concurrent job loops cannot splice a line. Lifecycle lines come from the
    single `emit()` funnel, so the log cannot drift from the event stream.
    Refusal lines carry `reason=` from `internal/fault`, never from matching
    error prose. **Level is a STREAM, not a journald priority:** INFO and below
    go to stdout, WARN and above to stderr, because journald derives PRIORITY
    from the stream and a ` WARN ` token in the text is just characters. The
    residual limit, stated honestly: systemd still assigns the whole unit ONE
    priority, so `journalctl -p warning` does NOT return our warnings. What
    works today is
    `journalctl --user -u pi-supervisor -o cat | grep ' WARN \| ERROR '`.
    Genuine per-record priorities need a `/dev/log` datagram, which issue #1
    lists under non-goals.
29. **Task completions are JSON-confirmed, never prose-claimed (ADR-0014).**
    The trigger is ONLY a `TaskUpdate` execution (`tool_execution_start`/
    `end` correlated by toolCallId, `args.status == "completed"`, non-error
    end); the plugin's tasks JSON confirms it, and `task_completed`/
    `task_lookup_failed` are NON-TERMINAL events through the one emit funnel.
    Non-terminal means the JOB keeps running — never that the watch holds:
    the watch CLIENT releases on both (exit 0, re-arm footer) so the LLM
    wakes, trust-but-verifies the task, and re-arms (owner decision).
    The store adapter (internal/taskwatch) mirrors pi-tasks 0.9.0's resolver
    exactly — PI_TASKS off/absolute/dot/named, tasks-config merge, scope
    session/project/session-global, PI_CODING_AGENT_DIR — and never guesses a
    file: memory/off is unavailable, and identity comes from the RUNNING
    session (get_state reply or the pinned transcript's `<ts>_<id>.jsonl
    name). Never re-open with a held handle (the plugin saves tmp+rename),
    never mutate the store, never derive status counts from events (status
    counts read the CURRENT list on demand; failed lookups are fallback 0/0
    plus a fault reason, a valid empty list is 0/0 with no error), and no
    pending task event may be lost to a round's terminal close (bounded
    flush before classification).

## State-durability invariants

These two are load-bearing and were both wrong at once (found via
`TestStopTwiceIsSafe`, which is now stable over 14/14 runs and 10/10 under
`-race`):

- **The round counter is persisted when a round STARTS**, not only when it
  ends. A daemon crash mid-round must not resume as if the round never ran.
- **`Stop()` writes the terminal state itself**, before returning, rather than
  leaving it to the round goroutine's exit path. `Stop()` returns before the
  round unwinds, so a caller reading the state file straight after a stop saw
  `running` — which also told a restarting daemon the job was still live. The
  loop's later write is idempotent.

Corollary: any code that changes a job's terminal state must persist it before
returning, not only on the goroutine that happens to notice.

## Build / test / install

```bash
GOTOOLCHAIN=auto go build ./... && go vet ./...   # both must be clean
GOTOOLCHAIN=auto go fix -diff ./...                # must print nothing (modernizers)
gofmt -l .                                         # must print nothing
# Live GitHub, READ-ONLY, opt-in — catches schema/shape errors fixtures cannot
PI_SUPERVISOR_LIVE_GH=1 PI_SUPERVISOR_LIVE_REPO=owner/name PI_SUPERVISOR_LIVE_PR=N \
  go test ./internal/review/ -run Live -v
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
