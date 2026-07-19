# Ago Apple clients

Native macOS renderer plus an iOS 18-compatible shared projection client. This is **not an iOS app** and not the complete Phase 2 product.

```sh
swift test --package-path ago-clients/apple
swift build --package-path ago-clients/apple
swift run --package-path ago-clients/apple AgoDesktop
```

`AgoDesktop` accepts a daemon base URL and thread ID, reads schema-v1 projection pages, and supports fenced queue edit/dequeue/steer, turn interrupt, and confirm/input/select dialog responses. Mutations are followed by an authoritative projection refresh; the renderer does not optimistically rewrite mailbox or dialog state. Queue edit currently resubmits the displayed opaque content (there is no structured JSON editor). The configurable `URLSession` transport may use plain HTTP and has no remote authentication. This is not a full Phase 2 client and has no provider/model controls, iOS application target, or deployment configuration.

## Closed macOS application bundle

Build a real release-mode `.app` without relying on a repository checkout, Homebrew, or a separately installed runtime after installation:

```sh
APP_VERSION=0.2.0 APP_BUILD_NUMBER=1 \
  ago-clients/apple/Scripts/build-macos-app.sh "$PWD/dist/AgoDesktop.app"
```

The deterministic assembly script builds the Swift `AgoDesktop` release product plus `ago` and `ago-supervisor` Go executables with path/build-ID stripping. It embeds those binaries, the current Mach-O Bun runtime, bundled Pi sidecar/provider JavaScript, the required Photon WASM asset, and the trusted plugin runtime under `Contents/Resources/Runtime`. Bun package code is bundled at build time, so `node_modules` is not required on the destination machine.

`Contents/Resources/bundle-manifest.json` is canonical JSON and the closed dependency/launch contract. Every path is relative to the application bundle root. In particular, a launcher must resolve `launch.daemon.executable` and every `pathArguments[].path` against `Bundle.main.bundleURL`; it must not resolve them through the current directory or `PATH`. Runtime database, socket, and user plugin configuration locations remain per-user writable data and are intentionally not embedded.

The manifest verifier checks the exact dependency inventory, asset checksums, executable bits, Mach-O format, dynamic library load paths, canonical relative launch paths, and absence of development-machine paths. In signed mode it also verifies each declared native component and the sealed application bundle:

```sh
python3 ago-clients/apple/ReleaseTools/bundle_manifest.py verify \
  --app dist/AgoDesktop.app
```

Credential-free structural coverage builds the complete bundle, loads the bundled provider asset with the bundled Bun runtime, proves manifest regeneration is byte-identical, rejects a missing provider dependency, and scans for repository/Homebrew absolute paths:

```sh
ago-clients/apple/Scripts/test-macos-bundle.sh
```

## Signed macOS test releases

`Scripts/release-macos.sh` first verifies the closed bundle manifest, then copies the `.app`, recursively signs every Mach-O component and the bundle with the hardened runtime and a trusted timestamp, and requires every declared native dependency to verify as signed. The bundled Bun runtime receives only its required JIT/unsigned-executable-memory entitlements. The lane then submits a ZIP to Apple's notary service, staples and validates the ticket, assesses it with Gatekeeper, and creates the distributable ZIP. It finally emits two canonical JSON manifests and deterministic Ed25519 signatures:

- `artifact-manifest.json` and `.sig`: sorted artifact names, byte sizes, and SHA-256 checksums;
- `update-channel.json` and `.sig`: an explicit channel/version/publication timestamp, HTTPS artifact URLs, and a checksum-bound reference to the artifact manifest.

The checked-in `ReleaseTools/update-public-key.pem` is the pinned update-channel trust root. Its private half is intentionally not in the repository. **Before the first externally distributed test release, replace this bootstrap public key in a reviewed change with the public half of the release key held by the credential owner.** A private key that does not match the selected public key is rejected. Rotation therefore requires shipping/reviewing the new public key before signing a channel with it; never fetch a replacement key from the update server.

Required external credentials:

1. `MACOS_CODESIGN_IDENTITY`: an installed `Developer ID Application` identity and private key.
2. `NOTARY_KEYCHAIN_PROFILE`: a profile created outside the repository with `xcrun notarytool store-credentials` (Apple ID/app-specific password/team ID or App Store Connect API key).
3. `UPDATE_SIGNING_PRIVATE_KEY`: path to the external Ed25519 PEM private key matching `UPDATE_PUBLIC_KEY` (which defaults to the bundled key).

All other release inputs are explicit. `PUBLISHED_AT` should be an RFC 3339 UTC value chosen once by release automation; the tool never reads the clock, so identical artifacts, metadata, and key produce byte-identical manifests and Ed25519 signatures.

```sh
export APP_PATH="$PWD/build/AgoDesktop.app"
export VERSION="0.2.0-test.1"
export CHANNEL="test"
export PUBLISHED_AT="2026-07-19T12:00:00Z"
export RELEASE_BASE_URL="https://updates.example.com/ago/test"
export OUTPUT_DIR="$PWD/dist/0.2.0-test.1"
export MACOS_CODESIGN_IDENTITY="Developer ID Application: Example Corp (TEAMID1234)"
export NOTARY_KEYCHAIN_PROFILE="ago-notary"
export UPDATE_SIGNING_PRIVATE_KEY="/secure/path/ago-update-ed25519.pem"
ago-clients/apple/Scripts/release-macos.sh
```

The lane checks every required variable, credential path, and update key pair before creating output or copying/signing the app. It does not allow ad-hoc signing, skip notarization, or publish unsigned metadata. Notarization requires network access and supported Xcode command-line tools.

Consumers should download all four manifest files, then verify them and the downloaded artifact before installing:

```sh
python3 ago-clients/apple/ReleaseTools/release_manifest.py verify \
  --release-dir dist/0.2.0-test.1 \
  --artifact-dir dist/0.2.0-test.1
```

The verifier fails closed on an invalid signature, unknown key ID/schema/field, non-canonical JSON, unsafe artifact name, non-HTTPS URL, mismatched cross-manifest data, symlink artifact, size mismatch, or checksum mismatch.

Credential-free local verification generates a fresh fixture keypair, proves deterministic output, verifies valid files, rejects artifact/manifest tampering, and confirms the release lane rejects missing credentials:

```sh
ago-clients/apple/Scripts/test-release-tools.sh
```
