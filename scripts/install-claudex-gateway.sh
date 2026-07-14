#!/bin/sh
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)"
EXPECTED="$HOME/orca/projects/x"
SOURCE="$ROOT/adapter/model-filter-proxy.mjs"
TARGET="$HOME/.local/share/cliproxyapi/model-filter-proxy.mjs"
BACKUP="$TARGET.backup.$(date +%Y%m%d%H%M%S)"
LABEL="local.cliproxyapi.model-filter"
DOMAIN="gui/$(id -u)"

if [ "$ROOT" != "$EXPECTED" ]; then
  echo "refusing non-canonical install: source=$ROOT expected=$EXPECTED" >&2
  exit 2
fi

cd "$ROOT"
node --test adapter/model-filter-proxy.test.mjs

if command -v lsof >/dev/null 2>&1 && \
  lsof -nP -iTCP:8318 -sTCP:ESTABLISHED 2>/dev/null | grep -q '127.0.0.1'; then
  echo "refusing gateway restart while 127.0.0.1:8318 has an active request; retry when the current model call finishes" >&2
  exit 3
fi

mkdir -p "$(dirname "$TARGET")"
if [ -f "$TARGET" ]; then
  cp -p "$TARGET" "$BACKUP"
  echo "backup: $BACKUP"
fi

TMP="$TARGET.tmp.$$"
trap 'rm -f "$TMP"' EXIT INT TERM
cp "$SOURCE" "$TMP"
chmod 0644 "$TMP"
mv -f "$TMP" "$TARGET"

launchctl kickstart -k "$DOMAIN/$LABEL"

for _ in 1 2 3 4 5; do
  if nc -z 127.0.0.1 8318 2>/dev/null; then
    echo "installed: $TARGET"
    echo "compaction route: gpt-5.6-sol -> gpt-5.6-luna"
    exit 0
  fi
  sleep 1
done

echo "gateway did not return on 127.0.0.1:8318" >&2
exit 1
