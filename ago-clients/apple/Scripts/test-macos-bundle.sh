#!/bin/bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
apple_dir=$(cd "$script_dir/.." && pwd)
tool="$apple_dir/ReleaseTools/bundle_manifest.py"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ago-bundle-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
app="$tmp/AgoDesktop.app"

"$script_dir/build-macos-app.sh" "$app"
python3 "$tool" verify --app "$app"
cp "$app/Contents/Resources/bundle-manifest.json" "$tmp/manifest-before.json"
python3 "$tool" create --app "$app" >/dev/null
cmp "$tmp/manifest-before.json" "$app/Contents/Resources/bundle-manifest.json"
provider_output=$(printf '{}\n' | "$app/Contents/Resources/Runtime/bin/bun" \
  "$app/Contents/Resources/Runtime/pi-adapter/provider-process.ts" 2>/dev/null || true)
printf '%s\n' "$provider_output" | rg -q '"type":"error"'

if rg -a -l '/Users/|/opt/homebrew|/usr/local/' \
  "$app/Contents/Info.plist" \
  "$app/Contents/Resources/bundle-manifest.json" \
  "$app/Contents/Resources/Runtime/pi-adapter" \
  "$app/Contents/Resources/Runtime/plugin-runtime"; then
  echo "error: bundle resources contain a development-machine absolute path" >&2
  exit 1
fi

if python3 "$tool" verify --app "$app" --require-signed >/dev/null 2>&1; then
  echo "error: unsigned native dependencies were accepted" >&2
  exit 1
fi
while IFS= read -r -d '' candidate; do
  if [[ -f "$candidate" ]] && file -b "$candidate" | rg -q 'Mach-O'; then
    if [[ "$candidate" == */Contents/Resources/Runtime/bin/bun ]]; then
      codesign --force --options runtime --sign - \
        --entitlements "$apple_dir/ReleaseTools/bun-runtime.entitlements" "$candidate"
    else
      codesign --force --options runtime --sign - "$candidate"
    fi
  fi
done < <(find "$app/Contents" -depth -mindepth 1 -print0)
codesign --force --options runtime --sign - \
  --entitlements "$apple_dir/ReleaseTools/release.entitlements" "$app"
python3 "$tool" verify --app "$app" --require-signed
codesign --remove-signature "$app/Contents/Resources/Runtime/bin/ago-supervisor"
if python3 "$tool" verify --app "$app" --require-signed >/dev/null 2>&1; then
  echo "error: unsigned declared native dependency was accepted" >&2
  exit 1
fi

rm "$app/Contents/Resources/Runtime/pi-adapter/provider-process.ts"
if python3 "$tool" verify --app "$app" >/dev/null 2>&1; then
  echo "error: missing provider runtime dependency was accepted" >&2
  exit 1
fi

echo "macOS bundle structural tests passed"
