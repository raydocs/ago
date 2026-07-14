#!/bin/sh
# T13: measure orchestrator.md size only (no rewrite).
set -eu
PATH_FILE="${1:-$HOME/.config/claudex/orchestrator.md}"
if [ ! -f "$PATH_FILE" ]; then
  echo "missing $PATH_FILE" >&2
  exit 1
fi
BYTES=$(wc -c < "$PATH_FILE" | tr -d ' ')
LINES=$(wc -l < "$PATH_FILE" | tr -d ' ')
# rough token estimate: bytes/4
TOKENS=$((BYTES / 4))
printf '{\n  "path": "%s",\n  "bytes": %s,\n  "lines": %s,\n  "approx_tokens_bytes_div_4": %s,\n  "note": "measurement only; do not split until core markers still load"\n}\n' \
  "$PATH_FILE" "$BYTES" "$LINES" "$TOKENS"
