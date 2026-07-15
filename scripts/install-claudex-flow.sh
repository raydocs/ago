#!/bin/sh
set -eu

VERSION="${1:-1.7.9}"
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)"
BIN_DIR="${CLAUDEX_BIN_DIR:-$HOME/.local/bin}"
CONFIG_DIR="${CLAUDEX_CONFIG_DIR:-$HOME/.config/claudex}"
TARGET="$BIN_DIR/claudex-flow"
WRAPPER_TARGET="$BIN_DIR/claudex"
ORCH="$CONFIG_DIR/orchestrator.md"
SETTINGS="$CONFIG_DIR/settings.json"
MCP="$CONFIG_DIR/mcp.json"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
BACKUP_DIR="$CONFIG_DIR/backups/$STAMP"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/claudex-install.XXXXXX")"

cleanup() { rm -rf "$TMP_DIR"; }
trap cleanup EXIT INT TERM

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "claudex: required command not found: $1" >&2
    exit 2
  }
}

backup_if_changed() {
  src="$1"
  managed="$2"
  if [ -f "$src" ] && ! cmp -s "$src" "$managed"; then
    mkdir -p "$BACKUP_DIR"
    cp -p "$src" "$BACKUP_DIR/$(basename "$src")"
  fi
}

backup_existing() {
  src="$1"
  if [ -f "$src" ]; then
    mkdir -p "$BACKUP_DIR"
    cp -p "$src" "$BACKUP_DIR/$(basename "$src")"
  fi
}

need go
need node
need claude

if [ ! -f "$ROOT/go.mod" ] || ! grep -q '^module claudexflow$' "$ROOT/go.mod"; then
  echo "claudex: run this installer from a claudex-flow source checkout" >&2
  exit 2
fi

cd "$ROOT"
go test ./...
go vet ./...
node --test adapter/model-filter-proxy.test.mjs

GOOS="$(go env GOOS)"
GOARCH="$(go env GOARCH)"
DIST_DIR="$ROOT/dist"
ARTIFACT="$DIST_DIR/claudex-flow-${VERSION}-${GOOS}-${GOARCH}"
BUILD_SOURCE="github.com/raydocs/claudex-flow@v${VERSION}"

mkdir -p "$BIN_DIR" "$CONFIG_DIR" "$DIST_DIR" "$ROOT/outputs"
go build -trimpath -ldflags "-s -w -X main.version=$VERSION -X main.buildSource=$BUILD_SOURCE" \
  -o "$TMP_DIR/claudex-flow" ./cmd/claudex-flow
chmod 0755 "$TMP_DIR/claudex-flow"

# Prepare every managed file before mutating the live installation.
cp "$ROOT/scripts/claudex" "$TMP_DIR/claudex"
chmod 0755 "$TMP_DIR/claudex"
cp "$ROOT/config/claudex/orchestrator.md" "$TMP_DIR/orchestrator.md"
printf '%s\n' '{}' > "$TMP_DIR/settings.json"
cat > "$TMP_DIR/mcp.json" <<EOF
{
  "mcpServers": {
    "claudex-flow": {
      "command": "$TARGET",
      "args": ["mcp"]
    }
  }
}
EOF

# Preserve user configuration. settings.json is merged in place by configure-hooks;
# the dedicated orchestrator and strict MCP registry are replaced after backup.
backup_if_changed "$ORCH" "$TMP_DIR/orchestrator.md"
backup_if_changed "$MCP" "$TMP_DIR/mcp.json"
backup_existing "$SETTINGS"
if [ ! -f "$SETTINGS" ]; then
  install -m 0600 "$TMP_DIR/settings.json" "$SETTINGS"
fi

install -m 0755 "$TMP_DIR/claudex-flow" "$TARGET"
install -m 0755 "$TMP_DIR/claudex" "$WRAPPER_TARGET"
install -m 0600 "$TMP_DIR/orchestrator.md" "$ORCH"
install -m 0600 "$TMP_DIR/mcp.json" "$MCP"
install -m 0755 "$TMP_DIR/claudex-flow" "$ARTIFACT"

CLAUDEX_SETTINGS="$SETTINGS" CLAUDEX_FLOW_BINARY="$TARGET" \
  CLAUDEX_MCP_CONFIG="$MCP" CLAUDEX_ORCHESTRATOR="$ORCH" "$TARGET" configure-hooks
CLAUDEX_SETTINGS="$SETTINGS" "$TARGET" version
CLAUDEX_SETTINGS="$SETTINGS" CLAUDEX_FLOW_BINARY="$TARGET" \
  CLAUDEX_MCP_CONFIG="$MCP" CLAUDEX_ORCHESTRATOR="$ORCH" "$TARGET" contract-guard
CLAUDEX_SETTINGS="$SETTINGS" "$TARGET" contract > "$ROOT/outputs/runtime-contract-$VERSION.json"

if command -v shasum >/dev/null 2>&1; then
  (
    cd "$DIST_DIR"
    shasum -a 256 "$(basename "$ARTIFACT")"
  ) > "$ROOT/outputs/claudex-flow-$VERSION.sha256"
  cp "$ROOT/outputs/claudex-flow-$VERSION.sha256" "$DIST_DIR/claudex-flow-$VERSION.sha256"
fi

echo "installed claudex-flow $VERSION"
echo "  launcher: $WRAPPER_TARGET"
echo "  runtime:  $TARGET"
echo "  config:   $CONFIG_DIR"
if [ -d "$BACKUP_DIR" ]; then
  echo "  backup:   $BACKUP_DIR"
fi
