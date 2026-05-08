#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

DEST="${DEST:-$HOME/bin}"
mkdir -p "$DEST"

go build -o whatsapp-cli .
install -m 0755 whatsapp-cli "$DEST/whatsapp-cli"

echo "Installed: $DEST/whatsapp-cli"
