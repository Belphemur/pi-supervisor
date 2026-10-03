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
ln -sfn "${SKILL_SRC}/pi-supervisor" "${SKILL_LINK_DIR}/pi-supervisor"

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
