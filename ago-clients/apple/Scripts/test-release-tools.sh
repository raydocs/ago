#!/bin/bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
apple_dir=$(cd "$script_dir/.." && pwd)
tool="$apple_dir/ReleaseTools/release_manifest.py"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ago-release-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

openssl genpkey -algorithm ED25519 -out "$tmp/private.pem" >/dev/null 2>&1
openssl pkey -in "$tmp/private.pem" -pubout -out "$tmp/public.pem" >/dev/null 2>&1
printf 'fixture artifact\n' > "$tmp/AgoDesktop-0.0.0-test-macOS.zip"
python3 "$tool" check-key --private-key "$tmp/private.pem" --public-key "$tmp/public.pem" >/dev/null

create_manifest() {
  local destination=$1
  python3 "$tool" create \
    --version 0.0.0-test \
    --channel test \
    --published-at 2026-01-01T00:00:00Z \
    --base-url https://updates.example.invalid/ago/test \
    --artifact "$tmp/AgoDesktop-0.0.0-test-macOS.zip" \
    --private-key "$tmp/private.pem" \
    --public-key "$tmp/public.pem" \
    --output-dir "$destination" >/dev/null
}

create_manifest "$tmp/first"
create_manifest "$tmp/second"
diff -ru "$tmp/first" "$tmp/second"
python3 "$tool" verify --release-dir "$tmp/first" --artifact-dir "$tmp" --public-key "$tmp/public.pem"

printf 'tampered\n' >> "$tmp/AgoDesktop-0.0.0-test-macOS.zip"
if python3 "$tool" verify --release-dir "$tmp/first" --artifact-dir "$tmp" --public-key "$tmp/public.pem" >/dev/null 2>&1; then
  echo "error: tampered artifact was accepted" >&2
  exit 1
fi

cp -R "$tmp/first" "$tmp/tampered-manifest"
python3 - "$tmp/tampered-manifest/update-channel.json" <<'PY'
import json, pathlib, sys
path = pathlib.Path(sys.argv[1])
value = json.loads(path.read_text())
value["channel"] = "stable"
path.write_text(json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n")
PY
if python3 "$tool" verify --release-dir "$tmp/tampered-manifest" --public-key "$tmp/public.pem" >/dev/null 2>&1; then
  echo "error: tampered manifest was accepted" >&2
  exit 1
fi

if env -i PATH="$PATH" /bin/bash "$script_dir/release-macos.sh" >/dev/null 2>&1; then
  echo "error: release lane did not fail closed without credentials" >&2
  exit 1
fi

echo "release tooling tests passed"
