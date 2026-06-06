#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

DEST="${DEST:-$HOME/bin}"
mkdir -p "$DEST"

# Derive a version from git so the binary reports the actual fork build
# (e.g. "1.3.3-5-g9d52db3", with "-dirty" appended for an uncommitted tree).
# Falls back to whatever is baked into main.version if git is unavailable.
VERSION="$(git describe --tags --dirty --always 2>/dev/null | sed 's/^v//')"

BUILD_FLAGS=()
if [[ -n "$VERSION" ]]; then
  BUILD_FLAGS+=(-ldflags "-X main.version=$VERSION")
fi

go build "${BUILD_FLAGS[@]}" -o whatsapp-cli .
install -m 0755 whatsapp-cli "$DEST/whatsapp-cli"

echo "Installed: $DEST/whatsapp-cli (version ${VERSION:-baked-in})"
