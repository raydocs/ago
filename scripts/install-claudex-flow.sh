#!/bin/sh
set -eu

VERSION="${1:-1.4.5}"
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)"
EXPECTED="$HOME/orca/projects/x"
TARGET="$HOME/.local/bin/claudex-flow"
WRAPPER_TARGET="$HOME/.local/bin/claudex"
TMP="$TARGET.tmp.$$"
WRAPPER_TMP="$WRAPPER_TARGET.tmp.$$"

cleanup() {
  rm -f "$TMP" "$WRAPPER_TMP"
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

GOOS="$(go env GOOS)"
GOARCH="$(go env GOARCH)"
DIST_DIR="$ROOT/dist"
mkdir -p "$DIST_DIR" "$(dirname "$TARGET")"
ARTIFACT="$DIST_DIR/claudex-flow-${VERSION}-${GOOS}-${GOARCH}"

go build -trimpath -ldflags "-s -w -X main.version=$VERSION -X main.buildSource=$ROOT" -o "$TMP" ./cmd/claudex-flow
chmod 0755 "$TMP"

# Immutable versioned artifact (checksum target); install copy is mutable path.
cp -f "$TMP" "$ARTIFACT"
chmod 0755 "$ARTIFACT"
mv -f "$TMP" "$TARGET"

# Install claudex wrapper atomically (required for --from-handoff recovery).
if [ -f "$ROOT/scripts/claudex" ]; then
  cp -f "$ROOT/scripts/claudex" "$WRAPPER_TMP"
  chmod 0755 "$WRAPPER_TMP"
  mv -f "$WRAPPER_TMP" "$WRAPPER_TARGET"
fi

# Bump contract marker in installed orchestrator if present (contract-guard).
ORCH="$HOME/.config/claudex/orchestrator.md"
if [ -f "$ORCH" ]; then
  # portable-ish: replace any claudex-workflow.v* contract line marker
  if command -v perl >/dev/null 2>&1; then
    perl -i -pe "s/claudex-workflow\\.v[0-9.]+/claudex-workflow.v${VERSION}/g" "$ORCH"
  elif command -v sed >/dev/null 2>&1; then
    sed -i.bak "s/claudex-workflow\\.v[0-9.][0-9.]*/claudex-workflow.v${VERSION}/g" "$ORCH" && rm -f "${ORCH}.bak"
  fi
fi

"$TARGET" configure-hooks

"$TARGET" version
"$TARGET" contract-guard
"$TARGET" contract > "$ROOT/outputs/runtime-contract-$VERSION.json"

if command -v shasum >/dev/null 2>&1; then
  # Checksum the immutable dist artifact, not the mutable ~/.local/bin path.
  (
    cd "$DIST_DIR"
    shasum -a 256 "$(basename "$ARTIFACT")"
  ) > "$ROOT/outputs/claudex-flow-$VERSION.sha256"
  # Also keep a copy next to the binary for release packaging.
  cp -f "$ROOT/outputs/claudex-flow-$VERSION.sha256" "$DIST_DIR/claudex-flow-$VERSION.sha256"
fi

echo "installed $TARGET and $WRAPPER_TARGET from $ROOT"
echo "artifact $ARTIFACT"
echo "checksum $(cat "$ROOT/outputs/claudex-flow-$VERSION.sha256" 2>/dev/null || true)"
