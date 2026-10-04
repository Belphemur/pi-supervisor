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

echo "==> enabling service"
systemctl --user daemon-reload
systemctl --user enable --now pi-supervisor.service

# Shell completion is part of the install (ADR-0008): detect every shell on
# PATH (bash/zsh/fish/powershell) and wire it. Idempotent — the guarded rc
# block is only added once, and re-running never duplicates it. Non-fatal: a
# headless box with no shell to configure must still install the daemon.
echo "==> shell completion"
if ! "${BIN_DIR}/pi-supervisor" completion install; then
    echo "    completion install skipped (no supported shell on PATH?)"
fi

echo "==> verification"
sleep 2
systemctl --user is-active pi-supervisor.service
"${BIN_DIR}/pi-supervisor" status
echo "==> install complete"
