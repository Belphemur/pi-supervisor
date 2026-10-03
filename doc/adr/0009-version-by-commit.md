# ADR-0009: Version by commit id

## Status

Implemented. `install.sh` injects the short git commit; dev builds report
`dev`.

## Context

The pi-supervisor CLI needed a `version` subcommand, but there was no version
string to report. The binary is built by `install.sh` from a git checkout, so
the commit id is the natural version — it is precise, reproducible, and ties
exactly to the source that produced the running daemon.

## Decision

- A new `internal/version` package holds a package-level `ver = "dev"`.
- `install.sh` overrides it at link time:

  ```
  GIT_REV="$(cd "$REPO_DIR" && git rev-parse --short HEAD 2>/dev/null || echo dev)"
  go build -trimpath -ldflags="-s -w -X pi-supervisor/internal/version.ver=${GIT_REV}" \
      -o "${BIN_DIR}/pi-supervisor" ./cmd/pi-supervisor
  ```

- The `pi-supervisor version` subcommand calls `version.Version()`, which
  returns `ver` (the linker-injected value, or `"dev"` when no `-X` was
  passed — e.g. a developer `go build`).
- viper is given a `version` default of `"dev"` so config files and the
  subcommand's help text are consistent with the no-injection case.

## Consequences

- `pi-supervisor version` always prints something useful: the commit id on an
  installed binary, `dev` on a local build.
- The version is deterministic per build (same commit → same string); the
  byte-level binary is still not reproducible across machines due to Go's
  build-id, but that does not affect the version contract.
- A detached-HEAD or non-git build (e.g. module-cache install) falls back to
  `dev`, which is honest rather than fabricated.
- This does **not** implement semver — the project is versioned by commit id,
  not by a release tag, matching pi's own `--version` (short commit) and the
  "never tagged, never released" posture in AGENTS.md.
