# ADR-0008 — cobra + viper CLI, shell completion, exit-code contract

Status: accepted (2026-10-02)

## Context

The CLI grew organically as a hand-rolled `switch os.Args[1]` in
`cmd/pi-supervisor/main.go`. It has no completion, no per-command help, and
flag parsing is manual (`-n`/`--no-wait` filtered out of the steer text by
hand). Operators want tab-completion for job names and flags. The exit codes
are load-bearing — scripts and the test suite assert on them.

## Decision

### Cobra for the command tree; viper for config

`spf13/cobra` provides the command tree, per-command help, real flag parsing,
and shell-completion generation. `spf13/viper` binds configuration with the
precedence: explicit flag > `PI_SUPERVISOR_*` env > optional
`~/.config/pi-supervisor/config.yaml` > built-in default. `PI_SUPERVISOR_CONFIG`
overrides the config path; `PI_SUPERVISOR_SOCKET` overrides the socket; both
env vars are empty-string-safe.

This adds third-party deps to a previously dep-free module (cobra + viper and
their transitive tree, ~30 modules). That is an accepted cost for correct
completion, help, and config precedence — a hand-rolled reimplementation would
be more code with less fidelity.

### Exit codes are a preserved contract

- **2 (usage/validation):** missing or unknown subcommand, bad flag, missing
  required argument, **and "no daemon on the socket"**.
- **1 (runtime):** the daemon answered but refused (ok:false), or a malformed
  response.
- **0:** success.

Cobra's own dispatch/arg errors are classified as usage (exit 2) via
`isCobraUsageError`, which matches cobra's stable argument-validation message
prefixes. `errUsage` / `errUnreachable` sentinels cover the paths the commands
themselves raise. `watch` keeps its own exits inside `watchCtl` because it
streams and distinguishes three terminal conditions.

### Completion install

`pi-supervisor completion install` detects the shells on PATH (bash, zsh,
fish, powershell; the login shell `$SHELL` is always included first), generates
each completion script with cobra, writes it to that shell's conventional
completion directory, and — for shells that do not auto-load — appends an
idempotent, marker-guarded source block to the rc file. Re-running is safe (a
file already containing the start marker is left untouched). Paths derive from
a base directory overridable via `PI_SUPERVISOR_COMPLETION_HOME`, so tests and
packagers can retarget without touching the real home.

Per-shell generators (`completion bash`, `completion zsh`, …) print a script
to stdout for manual sourcing.

### Interrupt steering (ADR-0007)

`steer --interrupt` / `-i` is a cobra bool flag; it rides on the existing
control `Request.Interrupt` wire field. See ADR-0007.

## Consequences

- Every subcommand gains `--help`, and job-name completion comes from
  `~/.pi/supervisor/jobs/*.json` (never from the daemon, so tab-completion
  never blocks).
- The exit-code contract that `main_test.go` pins is preserved through error
  classification; a future cobra upgrade that changes these message prefixes
  would need `isCobraUsageError` updated.
- Completion install touches rc files, but only appends a guarded block and is
  idempotent.