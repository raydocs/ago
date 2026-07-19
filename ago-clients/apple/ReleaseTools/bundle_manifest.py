#!/usr/bin/env python3
"""Create and verify the closed AgoDesktop.app runtime dependency manifest."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import plistlib
import subprocess
import sys


COMPONENTS = (
    ("desktop", "native", "Contents/MacOS/AgoDesktop"),
    ("ago-daemon", "native", "Contents/Resources/Runtime/bin/ago"),
    ("ago-supervisor", "native", "Contents/Resources/Runtime/bin/ago-supervisor"),
    ("bun-runtime", "native", "Contents/Resources/Runtime/bin/bun"),
    ("pi-sidecar", "asset", "Contents/Resources/Runtime/pi-adapter/main.ts"),
    ("pi-provider", "asset", "Contents/Resources/Runtime/pi-adapter/provider-process.ts"),
    ("pi-photon-wasm", "asset", "Contents/Resources/Runtime/pi-adapter/photon-node/photon_rs_bg.wasm"),
    ("plugin-runtime", "asset", "Contents/Resources/Runtime/plugin-runtime/main.ts"),
)
FORBIDDEN_TEXT = (b"/opt/homebrew", b"/usr/local/", b"/Users/")


class BundleError(Exception):
    pass


def canonical_json(value: object) -> bytes:
    return (json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n").encode()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def safe_path(value: object) -> PurePosixPath:
    if not isinstance(value, str):
        raise BundleError("bundle path must be a string")
    path = PurePosixPath(value)
    if path.is_absolute() or ".." in path.parts or str(path) != value or not value.startswith("Contents/"):
        raise BundleError(f"unsafe bundle-relative path: {value!r}")
    return path


def component_records(app: Path) -> list[dict]:
    records = []
    for identifier, kind, relative in COMPONENTS:
        path = app / relative
        if not path.is_file() or path.is_symlink():
            raise BundleError(f"required bundle dependency is missing or unsafe: {relative}")
        if kind == "native" and not os.access(path, os.X_OK):
            raise BundleError(f"native dependency is not executable: {relative}")
        record = {"id": identifier, "kind": kind, "path": relative}
        if kind == "asset":
            record["sha256"] = sha256_file(path)
        else:
            record["integrity"] = "apple-code-signature"
        records.append(record)
    return records


def expected_launch() -> dict:
    return {
        "base": "bundle-root",
        "daemon": {
            "arguments": ["daemon"],
            "executable": "Contents/Resources/Runtime/bin/ago",
            "pathArguments": [
                {"flag": "--executor-command", "path": "Contents/Resources/Runtime/bin/bun"},
                {"flag": "--executor-entry", "path": "Contents/Resources/Runtime/pi-adapter/main.ts"},
                {"flag": "--supervisor-command", "path": "Contents/Resources/Runtime/bin/ago-supervisor"},
                {"flag": "--bun", "path": "Contents/Resources/Runtime/bin/bun"},
                {"flag": "--plugin-runtime", "path": "Contents/Resources/Runtime/plugin-runtime/main.ts"},
            ],
        },
        "desktop": {"executable": "Contents/MacOS/AgoDesktop"},
    }


def create(args: argparse.Namespace) -> None:
    app = args.app.resolve()
    if not app.is_dir() or app.suffix != ".app":
        raise BundleError("--app must be an existing .app bundle")
    manifest = {
        "components": component_records(app),
        "launch": expected_launch(),
        "schemaVersion": 1,
    }
    destination = app / "Contents/Resources/bundle-manifest.json"
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = destination.with_name(f".{destination.name}.tmp-{os.getpid()}")
    temporary.write_bytes(canonical_json(manifest))
    os.replace(temporary, destination)
    print(f"created {destination}")


def run(arguments: list[str]) -> subprocess.CompletedProcess[bytes]:
    try:
        return subprocess.run(arguments, capture_output=True, check=False)
    except FileNotFoundError as exc:
        raise BundleError(f"required tool is missing: {arguments[0]}") from exc


def is_macho(path: Path) -> bool:
    result = run(["file", "-b", str(path)])
    return result.returncode == 0 and b"Mach-O" in result.stdout


def verify_native(path: Path, require_signed: bool) -> None:
    if not is_macho(path):
        raise BundleError(f"native dependency is not Mach-O: {path}")
    dependencies = run(["otool", "-L", str(path)])
    if dependencies.returncode != 0:
        raise BundleError(f"cannot inspect native dependencies: {path}")
    if b"/opt/homebrew" in dependencies.stdout or b"/usr/local/" in dependencies.stdout or b"/Users/" in dependencies.stdout:
        raise BundleError(f"native dependency assumes a development-machine path: {path}")
    if require_signed:
        signature = run(["codesign", "--verify", "--strict", "--verbose=2", str(path)])
        if signature.returncode != 0:
            raise BundleError(f"native dependency is unsigned or invalid: {path}")
        details = run(["codesign", "--display", "--verbose=4", str(path)])
        if details.returncode != 0 or b"runtime" not in details.stderr:
            raise BundleError(f"native dependency lacks hardened-runtime signing: {path}")


def verify(args: argparse.Namespace) -> None:
    app = args.app.resolve()
    manifest_path = app / "Contents/Resources/bundle-manifest.json"
    try:
        raw = manifest_path.read_bytes()
        manifest = json.loads(raw)
    except (OSError, json.JSONDecodeError) as exc:
        raise BundleError(f"cannot read bundle manifest: {exc}") from exc
    if not isinstance(manifest, dict) or canonical_json(manifest) != raw:
        raise BundleError("bundle manifest is not canonical JSON")
    if set(manifest) != {"components", "launch", "schemaVersion"} or manifest["schemaVersion"] != 1:
        raise BundleError("unsupported bundle manifest schema")
    if manifest["launch"] != expected_launch():
        raise BundleError("bundle launch paths do not match the closed runtime layout")
    for section in (manifest["launch"]["desktop"], manifest["launch"]["daemon"]):
        safe_path(section["executable"])
    for item in manifest["launch"]["daemon"]["pathArguments"]:
        safe_path(item["path"])

    expected = component_records(app)
    if manifest["components"] != expected:
        raise BundleError("bundle dependency manifest does not match bundle contents")
    declared_native = set()
    for record in expected:
        path = app / safe_path(record["path"])
        if record["kind"] == "native":
            declared_native.add(path.resolve())
            verify_native(path, args.require_signed)
        else:
            data = path.read_bytes()
            if any(marker in data for marker in FORBIDDEN_TEXT):
                raise BundleError(f"runtime asset contains a development-machine path assumption: {path}")

    discovered_native = {path.resolve() for path in app.rglob("*") if path.is_file() and is_macho(path)}
    if discovered_native != declared_native:
        extras = sorted(str(path.relative_to(app)) for path in discovered_native - declared_native)
        missing = sorted(str(path.relative_to(app)) for path in declared_native - discovered_native)
        raise BundleError(f"native component inventory mismatch (extra={extras}, missing={missing})")

    info_path = app / "Contents/Info.plist"
    try:
        info = plistlib.loads(info_path.read_bytes())
    except (OSError, plistlib.InvalidFileException) as exc:
        raise BundleError(f"invalid Info.plist: {exc}") from exc
    if info.get("CFBundleExecutable") != "AgoDesktop" or info.get("CFBundlePackageType") != "APPL":
        raise BundleError("Info.plist does not describe the AgoDesktop application")
    if args.require_signed:
        signature = run(["codesign", "--verify", "--deep", "--strict", "--verbose=2", str(app)])
        if signature.returncode != 0:
            raise BundleError("application bundle signature is invalid")
    print(f"verified closed bundle with {len(expected)} declared dependencies: {app}")


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    commands = result.add_subparsers(dest="command", required=True)
    create_parser = commands.add_parser("create")
    create_parser.add_argument("--app", type=Path, required=True)
    create_parser.set_defaults(action=create)
    verify_parser = commands.add_parser("verify")
    verify_parser.add_argument("--app", type=Path, required=True)
    verify_parser.add_argument("--require-signed", action="store_true")
    verify_parser.set_defaults(action=verify)
    return result


def main() -> int:
    try:
        args = parser().parse_args()
        args.action(args)
    except (BundleError, KeyError, OSError, TypeError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
