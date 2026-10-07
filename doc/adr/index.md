# ADR Index — pi-supervisor

Architecture decision records live in this directory, one file per decision,
named `NNNN-slug.md`. This index is the single table of contents: **when you
add or revise an ADR, add or update its row here in the same commit.**

| # | Decision | Status | One-line summary |
|---|---|---|---|
| [0001](0001-go-supervisor-daemon.md) | pi-supervisor as a Go systemd daemon | Accepted | Bash resume loop replaced by a `--user` daemon over a Unix socket |
| [0002](0002-daemon-native-client-and-status.md) | Daemon-native RPC client + parallel-session status | Accepted | pi RPC client in Go; status reflects all parallel sessions |
| [0003](0003-session-notifications.md) | Daemon-to-Hermes session notifications | Accepted | Blocking `watch` streams events; no cronjob, no pulling |
| [0004](0004-ci-stall-awareness.md) | CI-stall awareness | Accepted | Detect the answer-code-review park; cap retries, intervene |
| [0005](0005-steer-acknowledgements.md) | Steer acknowledgements | Accepted | Every steer reports a real outcome via per-frame ack records |
| [0006](0006-run-metadata-pr-url.md) | PR URL as run metadata | Accepted | Best-effort transcript scrape; `""` = never linked, not an error |
| [0007](0007-steer-interrupt.md) | `steer --interrupt` | Accepted | Opt-in SIGINT of pi's group before delivering the frame; fail-loud |
| [0008](0008-cobra-viper-cli.md) | cobra + viper CLI, completion, exit codes | Accepted | 2 = usage/validation, 1 = refusal, 0 = success; idempotent completion |
| [0009](0009-version-by-commit.md) | Version by commit id | Accepted | `-ldflags` injection by install.sh; `dev` fallback |
| [0010](0010-clean-session-restart.md) | Clean-session restart + empty-turn stall | Accepted | `restart --fresh` quarantines the JSONL; empty-turn needs stall + no tool_use |
| [0011](0011-transcript-is-the-completion-surface.md) | Transcript is the completion surface | Accepted | Marker read live from assistant text blocks, never the run log |
| [0012](0012-review-subcommand.md) | Daemon-orchestrated review campaigns | Accepted | Six shim verbs, `PRRT_` ids, answer-before-resolve, ack-gated bulk close |
| [0013](0013-dogfooding-findings.md) | Dogfooding findings | Accepted | `fault.Kind` closed enum = one refusal vocabulary everywhere |
| [0014](0014-task-completion-watch.md) | TaskUpdate completion notifications | Accepted | JSON-confirmed task completions release `watch -t`; job keeps running |
| [0015](0015-watch-reconnect.md) | Watch survives a daemon restart | Accepted | Live streams reconnect with bounded backoff; precheck re-derives state |
| [0016](0016-manual-review-reentry.md) | Manual review re-entry on a done job | Accepted | `review --pr N` arms AND starts; campaign owns its round budget |
| [0017](0017-watch-driven-resume.md) | Watch-driven resume across restarts | Accepted | Only actively watched jobs auto-resume; daemon's own stop stays stopped |
| [0018](0018-review-own-session.md) | Review campaigns run in their own session | Accepted | `review_brief` clears the build pin at arm; campaign LAUNCHes fresh |
| [0019](0019-mid-round-thread-reminders.md) | Mid-round thread reminders | Accepted | Open threads re-steered into the live reviewing round; plain steer, no interrupt |