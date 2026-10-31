#!/usr/bin/env python3
"""Verify the cross-repository G0 fixture and generated-contract snapshot."""

import hashlib
import json
import argparse
from pathlib import Path
import sys


ENGINE = Path(__file__).resolve().parents[1]
PROJECTS = ENGINE.parent
REPOS = {
    "kora": ENGINE,
    "kora-cloud": PROJECTS / "kora-cloud",
    "kora-studio-mvp": PROJECTS / "kora-studio-mvp",
    "kora-ts-sdk": PROJECTS / "kora-ts-sdk",
}
MANIFEST = ENGINE / "docs/architecture/contract-fixture-manifest.json"


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def verify(relative: str, expected: str) -> None:
    repo, _, suffix = relative.partition("/")
    if repo not in REPOS or not suffix:
        raise ValueError(f"invalid repository path in manifest: {relative}")
    path = REPOS[repo] / suffix
    actual = digest(path)
    if actual != expected:
        raise ValueError(f"digest drift: {relative}: expected {expected}, got {actual}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--local-only",
        action="store_true",
        help="verify only Engine-owned artifacts (suitable for the Engine-only CI checkout)",
    )
    args = parser.parse_args()
    manifest = json.loads(MANIFEST.read_text(encoding="utf-8"))
    checked = 0
    for fixture in manifest["fixtures"]:
        for consumer in fixture["consumers"]:
            if args.local_only and consumer.partition("/")[0] != "kora":
                continue
            verify(consumer, fixture["sha256"])
            checked += 1
    for contract in manifest["engine_contracts"]:
        verify(contract["schema"], contract["schema_sha256"])
        verify(contract["typescript"], contract["typescript_sha256"])
        checked += 2
        for copy_key in ("embedded_ui_copy", "studio_copy", "sdk_copy"):
            if copy_key in contract:
                if args.local_only and copy_key != "embedded_ui_copy":
                    continue
                hash_key = f"{copy_key}_sha256"
                verify(contract[copy_key], contract[hash_key])
                checked += 1
    scope = "Engine-local" if args.local_only else "cross-repository"
    print(f"{scope} contract fixture manifest verified: {checked} artifacts across "
          f"{len(manifest['fixtures'])} fixtures and {len(manifest['engine_contracts'])} contract versions")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, KeyError, ValueError, json.JSONDecodeError) as error:
        print(f"contract fixture manifest check failed: {error}", file=sys.stderr)
        raise SystemExit(1)
