#!/usr/bin/env bash
# Fetch a pinned clone of suitenumerique/drive into e2e/.drive.
#
# The script is idempotent: it does nothing when e2e/.drive already holds a
# checkout. It tries a local copy first, from a scratch clone left over on
# this build machine, to save time. The network clone is the fallback, and
# it must work on its own on a machine with no local copy.
set -euo pipefail

DRIVE_REPO="https://github.com/suitenumerique/drive.git"
DRIVE_REF="v0.21.1"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
DEST="$E2E_DIR/.drive"

if [ -d "$DEST/.git" ]; then
  echo "fetch-drive: $DEST already present, skipping"
  exit 0
fi

copy_local() {
  local source="$1"
  echo "fetch-drive: copying local clone from $source"
  rm -rf "$DEST"
  cp -r "$source" "$DEST"
  chmod -R u+w "$DEST"
  if git -C "$DEST" checkout "$DRIVE_REF" > /dev/null 2>&1; then
    echo "fetch-drive: checked out $DRIVE_REF from the local copy"
    return 0
  fi
  echo "fetch-drive: local copy has no $DRIVE_REF tag, falling back to a network clone"
  rm -rf "$DEST"
  return 1
}

if [ -n "${DRIVE_LOCAL_SOURCE:-}" ] && [ -d "${DRIVE_LOCAL_SOURCE}/.git" ]; then
  copy_local "$DRIVE_LOCAL_SOURCE" && exit 0
fi

echo "fetch-drive: cloning $DRIVE_REPO at $DRIVE_REF"
git clone --branch "$DRIVE_REF" --depth 1 "$DRIVE_REPO" "$DEST"
