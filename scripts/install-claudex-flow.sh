#!/bin/sh
set -eu

VERSION="${1:-1.4.4}"
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)"
EXPECTED="$HOME/orca/projects/x"
TARGET="$HOME/.local/bin/claudex-flow"
TMP="$TARGET.tmp.$$"

cleanup() {
  rm -f "$TMP"
}
trap cleanup EXIT INT TERM

if [ "$ROOT" != "$EXPECTED" ]; then
  echo "refusing non-canonical build: source=$ROOT expected=$EXPECTED" >&2
  exit 2
fi

cd "$ROOT"
go test ./...
go vet ./...
node --test adapter/model-filter-proxy.test.mjs
mkdir -p "$(dirname "$TARGET")"
go build -trimpath -ldflags "-s -w -X main.version=$VERSION -X main.buildSource=$ROOT" -o "$TMP" ./cmd/claudex-flow

chmod 0755 "$TMP"
mv -f "$TMP" "$TARGET"

"$TARGET" configure-hooks

"$TARGET" version
"$TARGET" contract-guard
"$TARGET" contract > "$ROOT/outputs/runtime-contract-$VERSION.json"

if command -v shasum >/dev/null 2>&1; then
  shasum -a 256 "$TARGET" > "$ROOT/outputs/claudex-flow-$VERSION.sha256"
fi

echo "installed $TARGET from $ROOT"
