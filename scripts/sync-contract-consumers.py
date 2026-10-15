#!/usr/bin/env python3
"""Copy generated Engine v2 validators and refresh their source manifests."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
from datetime import date


ENGINE = Path(__file__).resolve().parents[1]
PROJECTS = Path(os.environ.get("KORA_PROJECTS_ROOT", ENGINE.parent)).resolve()
CONSUMERS = {
    "kora/ui/src/generated/engine-v2.ts": ENGINE / "ui/src/generated/engine-v2.ts",
    "kora-studio-mvp/src/generated/engine-v2.ts": PROJECTS / "kora-studio-mvp/src/generated/engine-v2.ts",
    "kora-ts-sdk/src/generated/engine-v2.ts": PROJECTS / "kora-ts-sdk/src/generated/engine-v2.ts",
}
MANIFESTS = (
    ENGINE / "ui/src/generated/manifest.ts",
    PROJECTS / "kora-studio-mvp/src/generated/engine-v2.manifest.ts",
    PROJECTS / "kora-ts-sdk/src/generated/manifest.ts",
)
FIXTURE_MANIFEST = ENGINE / "docs/architecture/contract-fixture-manifest.json"
SOURCE_TYPESCRIPT = ENGINE / "contract/generated/engine-v2.ts"
SOURCE_SCHEMA = ENGINE / "contract/schema/engine-v2.json"


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def source_revision(engine: Path = ENGINE) -> tuple[str, str]:
    revision = subprocess.run(
        ["git", "rev-parse", "HEAD"], cwd=engine, check=True,
        capture_output=True, text=True,
    ).stdout.strip()
    dirty = subprocess.run(
        ["git", "status", "--porcelain", "--untracked-files=all", "--", "contract", "cmd/contractgen"], cwd=engine, check=True,
        capture_output=True, text=True,
    ).stdout
    source_changes = [line for line in dirty.splitlines() if line[3:].strip('"').endswith(".go")]
    return revision, "uncommitted" if source_changes else "clean"


def render_manifest(revision: str, source_tree: str, artifact_hash: str) -> bytes:
    text = (
        "// This manifest pins the Engine source revision and bytes used to generate\n"
        "// engine-v2.ts. Regenerate it only when the Engine contract artifact changes.\n"
        "export const ENGINE_CONTRACT_MANIFEST = {\n"
        "  schemaVersion: 2,\n"
        f"  sourceRevision: '{revision}',\n"
        f"  sourceTree: '{source_tree}',\n"
        "  artifact: 'contract/generated/engine-v2.ts',\n"
        f"  artifactSHA256: '{artifact_hash}',\n"
        "} as const\n"
    )
    return text.encode("utf-8")


def expected_files(update_snapshot_date: bool) -> dict[Path, bytes]:
    source = SOURCE_TYPESCRIPT.read_bytes()
    artifact_hash = sha256(source)
    revision, source_tree = source_revision()
    files = {path: source for path in CONSUMERS.values()}
    manifest = render_manifest(revision, source_tree, artifact_hash)
    files.update({path: manifest for path in MANIFESTS})

    fixture_manifest = json.loads(FIXTURE_MANIFEST.read_text(encoding="utf-8"))
    if update_snapshot_date:
        fixture_manifest["snapshot_date"] = date.today().isoformat()
    consumer_fields = {
        "embedded_ui_copy": "kora/ui/src/generated/engine-v2.ts",
        "studio_copy": "kora-studio-mvp/src/generated/engine-v2.ts",
        "sdk_copy": "kora-ts-sdk/src/generated/engine-v2.ts",
    }
    for contract_entry in fixture_manifest["engine_contracts"]:
        if contract_entry["version"] != "v2":
            continue
        contract_entry["schema_sha256"] = sha256(SOURCE_SCHEMA.read_bytes())
        contract_entry["typescript_sha256"] = artifact_hash
        for field, manifest_path in consumer_fields.items():
            contract_entry[field] = manifest_path
            contract_entry[f"{field}_sha256"] = artifact_hash
    files[FIXTURE_MANIFEST] = (json.dumps(fixture_manifest, indent=2) + "\n").encode("utf-8")
    return files


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true", help="fail instead of writing drifted files")
    args = parser.parse_args()
    files = expected_files(update_snapshot_date=not args.check)
    stale = []
    for path, expected in files.items():
        try:
            current = path.read_bytes()
        except OSError:
            current = None
        if current == expected:
            continue
        stale.append(path)
        if not args.check:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(expected)
    if args.check and stale:
        for path in stale:
            print(f"generated Engine contract consumer is stale: {path}", file=sys.stderr)
        print("run `make sync-contracts` from a checkout with all first-party repositories", file=sys.stderr)
        return 1
    if stale:
        print(f"synchronized {len(CONSUMERS)} Engine v2 contract copies and manifests")
    else:
        print("Engine v2 contract copies and manifests are current")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
