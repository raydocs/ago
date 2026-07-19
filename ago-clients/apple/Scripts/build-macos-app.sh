#!/bin/bash
set -euo pipefail

fail() { printf 'error: %s\n' "$*" >&2; exit 1; }

for command in bun file git go python3 swift; do
  command -v "$command" >/dev/null 2>&1 || fail "$command is required"
done

[[ $# -eq 1 ]] || fail "usage: $0 OUTPUT_APP"
output_app=$1
[[ "$output_app" == *.app ]] || fail "output must end in .app"
[[ ! -e "$output_app" ]] || fail "output already exists: $output_app"
app_version=${APP_VERSION:-0.0.0}
app_build_number=${APP_BUILD_NUMBER:-1}
[[ "$app_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.-]+)?$ ]] || fail "invalid APP_VERSION"
[[ "$app_build_number" =~ ^[1-9][0-9]*$ ]] || fail "invalid APP_BUILD_NUMBER"

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
apple_dir=$(cd "$script_dir/.." && pwd)
repo_root=$(git -C "$apple_dir" rev-parse --show-toplevel)
bundle_tool="$apple_dir/ReleaseTools/bundle_manifest.py"
bun_path=${BUN_PATH:-$(command -v bun)}
[[ -f "$bun_path" ]] || fail "BUN_PATH is not a file"
file -b "$bun_path" | grep -q 'Mach-O' || fail "BUN_PATH must be a macOS Mach-O executable"

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/ago-app-build.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT
swift_scratch="$work_dir/swift"
go_bin="$work_dir/go-bin"
mkdir -p "$go_bin"

swift build --package-path "$apple_dir" --scratch-path "$swift_scratch" \
  --configuration release --product AgoDesktop
swift_bin=$(swift build --package-path "$apple_dir" --scratch-path "$swift_scratch" \
  --configuration release --show-bin-path)

(cd "$repo_root" && CGO_ENABLED=0 go build -trimpath -buildvcs=false \
  -ldflags='-s -w -buildid=' -o "$go_bin/ago" ./cmd/ago)
(cd "$repo_root" && CGO_ENABLED=0 go build -trimpath -buildvcs=false \
  -ldflags='-s -w -buildid=' -o "$go_bin/ago-supervisor" ./cmd/ago-supervisor)

stage="$work_dir/AgoDesktop.app"
runtime="$stage/Contents/Resources/Runtime"
mkdir -p "$stage/Contents/MacOS" "$runtime/bin" "$runtime/pi-adapter/photon-node" "$runtime/plugin-runtime"
cp "$swift_bin/AgoDesktop" "$stage/Contents/MacOS/AgoDesktop"
cp "$go_bin/ago" "$runtime/bin/ago"
cp "$go_bin/ago-supervisor" "$runtime/bin/ago-supervisor"
cp -L "$bun_path" "$runtime/bin/bun"
chmod 755 "$stage/Contents/MacOS/AgoDesktop" "$runtime/bin/ago" "$runtime/bin/ago-supervisor" "$runtime/bin/bun"

# Bundle TypeScript and package dependencies into self-contained Bun assets.
"$bun_path" build "$repo_root/pi-adapter/src/main.ts" --target=bun --minify \
  --outfile="$runtime/pi-adapter/main.ts"
"$bun_path" build "$repo_root/pi-adapter/src/provider-process.ts" --target=bun --minify \
  --outfile="$runtime/pi-adapter/provider-process.ts"
"$bun_path" build "$repo_root/plugin-runtime/main.ts" --target=bun --minify \
  --outfile="$runtime/plugin-runtime/main.ts"
cp "$repo_root/pi-adapter/node_modules/@silvia-odwyer/photon-node/photon_rs_bg.wasm" \
  "$runtime/pi-adapter/photon-node/photon_rs_bg.wasm"

# Bun preserves this WASM package's source __dirname; make it bundle-relative.
REPO_ROOT="$repo_root" RUNTIME_ROOT="$runtime" python3 - <<'PY'
import os, pathlib
source = os.path.join(os.environ["REPO_ROOT"], "pi-adapter/node_modules/@silvia-odwyer/photon-node")
replacement = 'import.meta.dir+"/photon-node"'
for name in ("main.ts", "provider-process.ts"):
    path = pathlib.Path(os.environ["RUNTIME_ROOT"]) / "pi-adapter" / name
    text = path.read_text()
    needle = f'var __dirname="{source}"'
    if needle not in text:
        raise SystemExit(f"expected Photon __dirname was not emitted in {name}")
    path.write_text(text.replace(needle, f"var __dirname={replacement}"))
PY

APP_BUNDLE="$stage" APP_VERSION="$app_version" APP_BUILD_NUMBER="$app_build_number" python3 - <<'PY'
import os, pathlib, plistlib
app = pathlib.Path(os.environ["APP_BUNDLE"])
info = {
    "CFBundleDevelopmentRegion": "en",
    "CFBundleExecutable": "AgoDesktop",
    "CFBundleIdentifier": "com.ago.desktop",
    "CFBundleInfoDictionaryVersion": "6.0",
    "CFBundleName": "AgoDesktop",
    "CFBundlePackageType": "APPL",
    "CFBundleShortVersionString": os.environ["APP_VERSION"],
    "CFBundleVersion": os.environ["APP_BUILD_NUMBER"],
    "LSMinimumSystemVersion": "15.0",
}
(app / "Contents/Info.plist").write_bytes(plistlib.dumps(info, fmt=plistlib.FMT_XML, sort_keys=True))
PY

python3 "$bundle_tool" create --app "$stage"
python3 "$bundle_tool" verify --app "$stage"
mkdir -p "$(dirname "$output_app")"
mv "$stage" "$output_app"
printf 'built closed AgoDesktop bundle: %s\n' "$output_app"
