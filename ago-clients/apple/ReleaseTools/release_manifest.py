#!/usr/bin/env python3
"""Create and verify canonical, Ed25519-signed Ago macOS release manifests."""

from __future__ import annotations

import argparse
import base64
import binascii
from datetime import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from urllib.parse import quote, urlparse


ROOT = Path(__file__).resolve().parent
DEFAULT_PUBLIC_KEY = ROOT / "update-public-key.pem"
NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")
VALUE_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._+-]*$")
RESERVED_NAMES = {
    "artifact-manifest.json",
    "artifact-manifest.json.sig",
    "update-channel.json",
    "update-channel.json.sig",
}


class ReleaseError(Exception):
    pass


def canonical_json(value: object) -> bytes:
    return (json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False) + "\n").encode()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def run_openssl(arguments: list[str], *, input_data: bytes | None = None) -> bytes:
    try:
        result = subprocess.run(
            ["openssl", *arguments], input=input_data, capture_output=True, check=False
        )
    except FileNotFoundError as exc:
        raise ReleaseError("openssl is required") from exc
    if result.returncode != 0:
        detail = result.stderr.decode(errors="replace").strip()
        raise ReleaseError(f"openssl failed: {detail or 'unknown error'}")
    return result.stdout


def public_der(public_key: Path) -> bytes:
    return run_openssl(["pkey", "-pubin", "-in", str(public_key), "-outform", "DER"])


def key_id(public_key: Path) -> str:
    return "sha256:" + hashlib.sha256(public_der(public_key)).hexdigest()


def require_key_pair(private_key: Path, public_key: Path) -> None:
    private_der = run_openssl(
        ["pkey", "-in", str(private_key), "-pubout", "-outform", "DER"]
    )
    if not hashlib.sha256(private_der).digest() == hashlib.sha256(public_der(public_key)).digest():
        raise ReleaseError("private key does not match the selected public key")


def sign(data: bytes, private_key: Path) -> bytes:
    with tempfile.NamedTemporaryFile() as data_file:
        data_file.write(data)
        data_file.flush()
        signature = run_openssl(
            ["pkeyutl", "-sign", "-rawin", "-inkey", str(private_key), "-in", data_file.name]
        )
    return base64.b64encode(signature) + b"\n"


def verify_signature(data: bytes, encoded_signature: bytes, public_key: Path) -> None:
    if not encoded_signature.endswith(b"\n") or b"\n" in encoded_signature[:-1]:
        raise ReleaseError("signature is not canonical base64")
    try:
        signature = base64.b64decode(encoded_signature[:-1], validate=True)
    except binascii.Error as exc:
        raise ReleaseError("signature is not canonical base64") from exc
    with tempfile.NamedTemporaryFile() as signature_file, tempfile.NamedTemporaryFile() as data_file:
        signature_file.write(signature)
        signature_file.flush()
        data_file.write(data)
        data_file.flush()
        run_openssl(
            [
                "pkeyutl",
                "-verify",
                "-rawin",
                "-pubin",
                "-inkey",
                str(public_key),
                "-sigfile",
                signature_file.name,
                "-in",
                data_file.name,
            ],
        )


def load_canonical(path: Path) -> tuple[dict, bytes]:
    try:
        raw = path.read_bytes()
        value = json.loads(raw)
    except (OSError, json.JSONDecodeError) as exc:
        raise ReleaseError(f"cannot read {path}: {exc}") from exc
    if not isinstance(value, dict) or canonical_json(value) != raw:
        raise ReleaseError(f"{path.name} is not canonical JSON")
    return value, raw


def valid_release_value(label: str, value: str) -> str:
    if not VALUE_RE.fullmatch(value):
        raise ReleaseError(f"invalid {label}: {value!r}")
    return value


def valid_published_at(value: str) -> str:
    if not re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z", value):
        raise ReleaseError("publishedAt must be an RFC 3339 UTC timestamp with whole seconds")
    try:
        datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ")
    except ValueError as exc:
        raise ReleaseError("publishedAt is not a valid calendar timestamp") from exc
    return value


def artifact_record(path: Path) -> dict:
    if not path.is_file() or path.is_symlink():
        raise ReleaseError(f"artifact must be a regular, non-symlink file: {path}")
    if not NAME_RE.fullmatch(path.name):
        raise ReleaseError(f"unsafe artifact name: {path.name!r}")
    if path.name in RESERVED_NAMES:
        raise ReleaseError(f"artifact uses a reserved manifest name: {path.name}")
    return {"name": path.name, "sha256": sha256_file(path), "size": path.stat().st_size}


def write_atomic(path: Path, data: bytes) -> None:
    temporary = path.with_name(f".{path.name}.tmp-{os.getpid()}")
    temporary.write_bytes(data)
    os.replace(temporary, path)


def check_key(args: argparse.Namespace) -> None:
    if not args.private_key.is_file() or not args.public_key.is_file():
        raise ReleaseError("both update signing keys are required")
    require_key_pair(args.private_key.resolve(), args.public_key.resolve())
    print(f"verified update signing key pair ({key_id(args.public_key.resolve())})")


def create(args: argparse.Namespace) -> None:
    private_key = args.private_key.resolve()
    public_key = args.public_key.resolve()
    if not private_key.is_file():
        raise ReleaseError("update signing private key is required")
    if not public_key.is_file():
        raise ReleaseError("update verification public key is required")
    require_key_pair(private_key, public_key)

    version = valid_release_value("version", args.version)
    channel = valid_release_value("channel", args.channel)
    parsed_url = urlparse(args.base_url)
    if parsed_url.scheme != "https" or not parsed_url.netloc or parsed_url.query or parsed_url.fragment:
        raise ReleaseError("base URL must be an HTTPS URL without a query or fragment")
    base_url = args.base_url.rstrip("/")

    records = sorted((artifact_record(path.absolute()) for path in args.artifact), key=lambda item: item["name"])
    if len({item["name"] for item in records}) != len(records):
        raise ReleaseError("artifact names must be unique")

    signer = key_id(public_key)
    artifact_manifest = {
        "artifacts": records,
        "keyId": signer,
        "schemaVersion": 1,
        "version": version,
    }
    artifact_bytes = canonical_json(artifact_manifest)
    artifact_manifest_name = "artifact-manifest.json"
    artifact_signature_name = "artifact-manifest.json.sig"
    update_manifest = {
        "artifactManifest": {
            "name": artifact_manifest_name,
            "sha256": hashlib.sha256(artifact_bytes).hexdigest(),
            "signature": artifact_signature_name,
        },
        "artifacts": [
            {**record, "url": f"{base_url}/{quote(record['name'])}"} for record in records
        ],
        "channel": channel,
        "keyId": signer,
        "publishedAt": valid_published_at(args.published_at),
        "schemaVersion": 1,
        "version": version,
    }
    update_bytes = canonical_json(update_manifest)

    output = args.output_dir.resolve()
    output.mkdir(parents=True, exist_ok=True)
    outputs = {
        artifact_manifest_name: artifact_bytes,
        artifact_signature_name: sign(artifact_bytes, private_key),
        "update-channel.json": update_bytes,
        "update-channel.json.sig": sign(update_bytes, private_key),
    }
    for name, data in outputs.items():
        write_atomic(output / name, data)
    print(f"created signed release manifests in {output}")


def expect_keys(value: dict, expected: set[str], label: str) -> None:
    if set(value) != expected:
        raise ReleaseError(f"{label} has unexpected or missing fields")


def validate_record(record: object, *, with_url: bool) -> dict:
    if not isinstance(record, dict):
        raise ReleaseError("artifact entry is not an object")
    expected = {"name", "sha256", "size"} | ({"url"} if with_url else set())
    expect_keys(record, expected, "artifact entry")
    if not isinstance(record["name"], str) or not NAME_RE.fullmatch(record["name"]):
        raise ReleaseError("artifact entry has an unsafe name")
    if not isinstance(record["sha256"], str) or not re.fullmatch(r"[0-9a-f]{64}", record["sha256"]):
        raise ReleaseError("artifact entry has an invalid SHA-256")
    if isinstance(record["size"], bool) or not isinstance(record["size"], int) or record["size"] < 0:
        raise ReleaseError("artifact entry has an invalid size")
    if with_url:
        parsed = urlparse(record["url"]) if isinstance(record["url"], str) else None
        if parsed is None or parsed.scheme != "https" or not parsed.netloc or parsed.query or parsed.fragment:
            raise ReleaseError("artifact URL must be HTTPS without a query or fragment")
        if parsed.path.rsplit("/", 1)[-1] != quote(record["name"]):
            raise ReleaseError("artifact URL does not end in the encoded artifact name")
    return record


def verify(args: argparse.Namespace) -> None:
    release_dir = args.release_dir.resolve()
    public_key = args.public_key.resolve()
    if not public_key.is_file():
        raise ReleaseError("bundled update verification public key is missing")

    artifact_manifest, artifact_bytes = load_canonical(release_dir / "artifact-manifest.json")
    update_manifest, update_bytes = load_canonical(release_dir / "update-channel.json")
    verify_signature(artifact_bytes, (release_dir / "artifact-manifest.json.sig").read_bytes(), public_key)
    verify_signature(update_bytes, (release_dir / "update-channel.json.sig").read_bytes(), public_key)

    expect_keys(artifact_manifest, {"artifacts", "keyId", "schemaVersion", "version"}, "artifact manifest")
    expect_keys(
        update_manifest,
        {"artifactManifest", "artifacts", "channel", "keyId", "publishedAt", "schemaVersion", "version"},
        "update manifest",
    )
    if artifact_manifest["schemaVersion"] != 1 or update_manifest["schemaVersion"] != 1:
        raise ReleaseError("unsupported manifest schema")
    expected_key_id = key_id(public_key)
    if artifact_manifest["keyId"] != expected_key_id or update_manifest["keyId"] != expected_key_id:
        raise ReleaseError("manifest key ID does not match the bundled public key")
    if artifact_manifest["version"] != update_manifest["version"]:
        raise ReleaseError("manifest versions do not match")
    valid_release_value("version", artifact_manifest["version"])
    valid_release_value("channel", update_manifest["channel"])
    if not isinstance(update_manifest["publishedAt"], str):
        raise ReleaseError("publishedAt must be a string")
    valid_published_at(update_manifest["publishedAt"])

    reference = update_manifest["artifactManifest"]
    if not isinstance(reference, dict):
        raise ReleaseError("artifactManifest reference is invalid")
    expect_keys(reference, {"name", "sha256", "signature"}, "artifact manifest reference")
    if reference != {
        "name": "artifact-manifest.json",
        "sha256": hashlib.sha256(artifact_bytes).hexdigest(),
        "signature": "artifact-manifest.json.sig",
    }:
        raise ReleaseError("artifact manifest reference does not match signed content")

    artifact_records = [validate_record(item, with_url=False) for item in artifact_manifest["artifacts"]]
    update_records = [validate_record(item, with_url=True) for item in update_manifest["artifacts"]]
    if artifact_records != sorted(artifact_records, key=lambda item: item["name"]):
        raise ReleaseError("artifact records are not sorted")
    projected = [{key: item[key] for key in ("name", "sha256", "size")} for item in update_records]
    if projected != artifact_records:
        raise ReleaseError("update artifacts do not match the artifact manifest")

    if args.artifact_dir is not None:
        artifact_dir = args.artifact_dir.resolve()
        for record in artifact_records:
            path = artifact_dir / record["name"]
            if not path.is_file() or path.is_symlink():
                raise ReleaseError(f"artifact is missing or unsafe: {record['name']}")
            if path.stat().st_size != record["size"] or sha256_file(path) != record["sha256"]:
                raise ReleaseError(f"artifact integrity check failed: {record['name']}")
    print(f"verified {len(artifact_records)} artifact(s) for {update_manifest['channel']} {update_manifest['version']}")


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    commands = result.add_subparsers(dest="command", required=True)
    key_parser = commands.add_parser("check-key", help="verify a private/public release key pair")
    key_parser.add_argument("--private-key", required=True, type=Path)
    key_parser.add_argument("--public-key", type=Path, default=DEFAULT_PUBLIC_KEY)
    key_parser.set_defaults(action=check_key)

    create_parser = commands.add_parser("create", help="create deterministic signed manifests")
    create_parser.add_argument("--version", required=True)
    create_parser.add_argument("--channel", required=True)
    create_parser.add_argument("--published-at", required=True, help="explicit RFC 3339 release timestamp")
    create_parser.add_argument("--base-url", required=True)
    create_parser.add_argument("--artifact", required=True, action="append", type=Path)
    create_parser.add_argument("--private-key", required=True, type=Path)
    create_parser.add_argument("--public-key", type=Path, default=DEFAULT_PUBLIC_KEY)
    create_parser.add_argument("--output-dir", required=True, type=Path)
    create_parser.set_defaults(action=create)

    verify_parser = commands.add_parser("verify", help="verify signatures, schema, and optional artifacts")
    verify_parser.add_argument("--release-dir", required=True, type=Path)
    verify_parser.add_argument("--artifact-dir", type=Path)
    verify_parser.add_argument("--public-key", type=Path, default=DEFAULT_PUBLIC_KEY)
    verify_parser.set_defaults(action=verify)
    return result


def main() -> int:
    try:
        args = parser().parse_args()
        args.action(args)
    except (ReleaseError, OSError, KeyError, TypeError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
