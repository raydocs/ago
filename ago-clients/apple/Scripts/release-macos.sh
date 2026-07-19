#!/bin/bash
set -euo pipefail

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_env() {
  local name=$1
  [[ -n "${!name:-}" ]] || fail "$name is required"
}

for command in codesign ditto openssl python3 spctl xcrun; do
  command -v "$command" >/dev/null 2>&1 || fail "$command is required"
done

require_env APP_PATH
require_env VERSION
require_env CHANNEL
require_env PUBLISHED_AT
require_env RELEASE_BASE_URL
require_env OUTPUT_DIR
require_env MACOS_CODESIGN_IDENTITY
require_env NOTARY_KEYCHAIN_PROFILE
require_env UPDATE_SIGNING_PRIVATE_KEY

[[ "$APP_PATH" == *.app ]] || fail "APP_PATH must name an .app bundle"
[[ -d "$APP_PATH" ]] || fail "APP_PATH does not exist: $APP_PATH"
[[ -f "$UPDATE_SIGNING_PRIVATE_KEY" ]] || fail "UPDATE_SIGNING_PRIVATE_KEY is not a file"

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
apple_dir=$(cd "$script_dir/.." && pwd)
release_tools="$apple_dir/ReleaseTools"
public_key=${UPDATE_PUBLIC_KEY:-"$release_tools/update-public-key.pem"}
[[ -f "$public_key" ]] || fail "update public key is not a file: $public_key"
python3 "$release_tools/release_manifest.py" check-key \
  --private-key "$UPDATE_SIGNING_PRIVATE_KEY" \
  --public-key "$public_key" >/dev/null
python3 "$release_tools/bundle_manifest.py" verify --app "$APP_PATH"

mkdir -p "$OUTPUT_DIR"
output_dir=$(cd "$OUTPUT_DIR" && pwd)
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/ago-release.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT

app_name=$(basename "$APP_PATH")
signed_app="$work_dir/$app_name"
ditto "$APP_PATH" "$signed_app"

# Sign nested Mach-O code from the inside out, then seal the app bundle itself.
while IFS= read -r -d '' candidate; do
  if [[ -f "$candidate" ]] && file -b "$candidate" | grep -q 'Mach-O'; then
    if [[ "$candidate" == */Contents/Resources/Runtime/bin/bun ]]; then
      codesign --force --options runtime --timestamp \
        --entitlements "$release_tools/bun-runtime.entitlements" \
        --sign "$MACOS_CODESIGN_IDENTITY" "$candidate"
    else
      codesign --force --options runtime --timestamp --sign "$MACOS_CODESIGN_IDENTITY" "$candidate"
    fi
  elif [[ -d "$candidate" && ( "$candidate" == *.framework || "$candidate" == *.xpc || "$candidate" == *.appex || "$candidate" == *.app ) ]]; then
    codesign --force --options runtime --timestamp --sign "$MACOS_CODESIGN_IDENTITY" "$candidate"
  fi
done < <(find "$signed_app/Contents" -depth -mindepth 1 -print0)

codesign --force --options runtime --timestamp --entitlements "$release_tools/release.entitlements" \
  --sign "$MACOS_CODESIGN_IDENTITY" "$signed_app"
codesign --verify --deep --strict --verbose=2 "$signed_app"
python3 "$release_tools/bundle_manifest.py" verify --app "$signed_app" --require-signed

pre_notary="$work_dir/notarization.zip"
ditto -c -k --sequesterRsrc --keepParent "$signed_app" "$pre_notary"
xcrun notarytool submit "$pre_notary" --keychain-profile "$NOTARY_KEYCHAIN_PROFILE" --wait
xcrun stapler staple "$signed_app"
xcrun stapler validate "$signed_app"
python3 "$release_tools/bundle_manifest.py" verify --app "$signed_app" --require-signed
spctl --assess --type execute --verbose=2 "$signed_app"

artifact_name="AgoDesktop-${VERSION}-macOS.zip"
artifact="$output_dir/$artifact_name"
rm -f "$artifact"
ditto -c -k --sequesterRsrc --keepParent "$signed_app" "$artifact"

python3 "$release_tools/release_manifest.py" create \
  --version "$VERSION" \
  --channel "$CHANNEL" \
  --published-at "$PUBLISHED_AT" \
  --base-url "$RELEASE_BASE_URL" \
  --artifact "$artifact" \
  --private-key "$UPDATE_SIGNING_PRIVATE_KEY" \
  --public-key "$public_key" \
  --output-dir "$output_dir"
python3 "$release_tools/release_manifest.py" verify \
  --release-dir "$output_dir" \
  --artifact-dir "$output_dir" \
  --public-key "$public_key"

printf 'signed, notarized, stapled, and manifested release: %s\n' "$artifact"
