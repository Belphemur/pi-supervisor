#!/usr/bin/env bash
# install.sh — install pi-supervisor as a systemd --user service, wire the
# Hermes skill via symlink, and verify the deployment.
#
# Usage:  ./install/install.sh          (from the repo root or anywhere)
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="${HOME}/.local/bin"
UNIT_DIR="${HOME}/.config/systemd/user"
SKILL_LINK_DIR="${HOME}/.hermes/skills/autonomous-ai-agents"
SKILL_SRC="${REPO_DIR}/doc/skill"

echo "==> building (Go toolchain per go.mod)"
GIT_REV="$(cd "$REPO_DIR" && git rev-parse --short HEAD 2>/dev/null || echo dev)"
( cd "$REPO_DIR" && GOTOOLCHAIN=auto go build -trimpath \
    -ldflags="-s -w -X pi-supervisor/internal/version.ver=${GIT_REV}" \
    -o "${BIN_DIR}/pi-supervisor" ./cmd/pi-supervisor )

echo "==> unit file"
mkdir -p "$UNIT_DIR" "$BIN_DIR"
cp "${REPO_DIR}/install/pi-supervisor.service" "$UNIT_DIR/pi-supervisor.service"

echo "==> Hermes skill symlink"
mkdir -p "$SKILL_LINK_DIR"
# The skill lives canonically in doc/skill/pi-supervisor/; the skills dir gets
# a symlink so edits land in one place.
if [ -e "${SKILL_LINK_DIR}/pi-supervisor" ] && [ ! -L "${SKILL_LINK_DIR}/pi-supervisor" ]; then
  echo "    existing real dir ${SKILL_LINK_DIR}/pi-supervisor found; replacing with symlink"
  rm -rf "${SKILL_LINK_DIR}/pi-supervisor"
fi
ln -sfn "${SKILL_SRC}/pi-supervisor" "$SKILL_LINK_DIR/pi-supervisor"

# The review shim (ADR-0012) is the ONLY way a pi review round reaches GitHub,
# so it is part of the install, not an optional extra: a review round whose
# transport is missing fails at the first verb. Installed as a symlink to the
# canonical copy in doc/skill/pi_supervisor_review/, so edits land in one place.
echo "==> review shim"
install -d "$BIN_DIR"
if [ -e "${BIN_DIR}/_pi-supervisor-review" ] && [ ! -L "${BIN_DIR}/_pi-supervisor-review" ]; then
  echo "    existing real file ${BIN_DIR}/_pi-supervisor-review found; replacing with symlink"
  rm -f "${BIN_DIR}/_pi-supervisor-review"
fi
ln -sfn "${SKILL_SRC}/pi_supervisor_review/_pi-supervisor-review" "$BIN_DIR/_pi-supervisor-review"
chmod +x "${SKILL_SRC}/pi_supervisor_review/_pi-supervisor-review"
# The shim resolves pi-supervisor from PATH; if the user's PATH misses BIN_DIR
# the shim would fail at exec time rather than at install time, so check here.
if ! command -v pi-supervisor >/dev/null 2>&1; then
  echo "    note: ${BIN_DIR} is not on PATH — the review shim will not resolve pi-supervisor."
  echo "          add it, or export PI_SUPERVISOR_BIN=${BIN_DIR}/pi-supervisor"
fi

# Hooks (ADR-0012 follow-up). This repo sets core.hooksPath=.githooks, where
# BOTH hooks are tracked in git:
#
#   pre-push   the compensating control for the unapplied ruleset (see below)
#   post-push  asks the daemon to compare each job's review baseline against the
#              PR's current open-thread count, because a push is what makes bots
#              re-review and new findings appear
#
# Nothing is copied: git runs the tracked files directly. The earlier version
# installed into .git/hooks, which git NEVER executed here, so the hook was
# installed and inert.
echo "==> git hooks"
HOOKS_PATH="$(git -C "$REPO_DIR" config core.hooksPath || true)"
if [ -z "$HOOKS_PATH" ]; then
  git -C "$REPO_DIR" config core.hooksPath .githooks
  echo "    set core.hooksPath=.githooks (was: unset)"
elif [ "$HOOKS_PATH" != ".githooks" ]; then
  # NEVER overwrite an existing hooks path: doing so silently disables whatever
  # hooks the developer already had there (pre-commit, commit-msg, ...).
  # Point at ours explicitly instead, and say so.
  echo "    core.hooksPath is '$HOOKS_PATH' — leaving it alone."
  echo "    to activate ours too: git config core.hooksPath .githooks"
fi
for hook in pre-push post-push; do
  if [ -f "${REPO_DIR}/.githooks/${hook}" ]; then
    chmod +x "${REPO_DIR}/.githooks/${hook}"
    echo "    active: .githooks/${hook}"
  fi
done

echo "==> enabling service"
systemctl --user daemon-reload
# `enable --now` is a NO-OP on an already-active unit: it enables the unit but
# never restarts a running one, so an installer run that rebuilt the binary left
# the OLD process serving. It then reported success, and the verification step
# below only checked `is-active` — which was true for the stale process. This bit
# the author during the ADR-0012 rollout: "active" + unreachable socket.
#
# Correct sequence: enable (idempotent), then restart (always picks up the new
# binary). `systemctl enable` without --now is safe on a never-installed unit.
systemctl --user enable pi-supervisor.service
systemctl --user restart pi-supervisor.service

# Shell completion is part of the install (ADR-0008): detect every shell on
# PATH (bash/zsh/fish/powershell) and wire it. Idempotent — the guarded rc
# block is only added once, and re-running never duplicates it. Non-fatal: a
# headless box with no shell to configure must still install the daemon.
echo "==> shell completion"
if ! "${BIN_DIR}/pi-supervisor" completion install; then
    echo "    completion install skipped (no supported shell on PATH?)"
fi

echo "==> verification"
# Wait for READY, not merely for the unit to be active: the socket appears only
# after LoadJobs + Serve succeed, and `is-active` is true a moment earlier.
READY=0
for _ in $(seq 1 30); do
    if "${BIN_DIR}/pi-supervisor" status >/dev/null 2>&1; then
        READY=1
        break
    fi
    sleep 1
done
systemctl --user is-active pi-supervisor.service
if [ "$READY" -ne 1 ]; then
    echo "    ERROR: daemon is active but its control socket never answered."
    echo "    journalctl --user -u pi-supervisor -n 30 --no-pager"
    exit 1
fi

# Prove the RUNNING process is the binary we just built. Without this the
# installer happily reports success while a stale process serves the socket —
# exactly the failure that prompted this check.
GIT_REV="$(cd "$REPO_DIR" && git rev-parse --short HEAD 2>/dev/null || echo dev)"
RUNNING_PID="$(systemctl --user show -p MainPID --value pi-supervisor.service)"
RUNNING_REV="dev"
if [ "$RUNNING_PID" != "0" ] && [ -r "/proc/$RUNNING_PID/exe" ]; then
    # The running binary's build-time version, via the version subcommand of the
    # binary ITSELF (a stale process cannot report the new commit).
    RUNNING_REV="$("${BIN_DIR}/pi-supervisor" version 2>/dev/null | awk '{print $2}')"
fi
echo "    built=${GIT_REV} serving=${RUNNING_REV} pid=${RUNNING_PID}"
if [ "$GIT_REV" != "dev" ] && [ "$RUNNING_REV" != "$GIT_REV" ]; then
    echo "    ERROR: the running daemon (${RUNNING_REV}) is not the binary just built (${GIT_REV})."
    echo "    The unit was not restarted, or something else is holding the socket."
    exit 1
fi

"${BIN_DIR}/pi-supervisor" status
echo "==> install complete"
