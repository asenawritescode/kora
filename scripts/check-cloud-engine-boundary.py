#!/usr/bin/env python3
"""Guard the normal Cloud build against Engine implementation imports.

The temporary compatibility adapter is compiled only with the explicit
``kora_legacy_engine`` build tag and is tested separately. The normal build
must use only the public contract and typed management client seams.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any


ENGINE_PREFIX = "github.com/asenawritescode/kora/"
PUBLIC_SEAMS = {"contract", "sdk/management"}
EXPECTED_LEGACY_IMPORTS: set[tuple[str, str]] = set()


def legacy_imports(graph: dict[str, Any]) -> set[tuple[str, str]]:
    result: set[tuple[str, str]] = set()
    for edge in graph.get("edges", []):
        if edge.get("from_module") != "cloud" or edge.get("kind") != "production":
            continue
        target = edge.get("to", "")
        if not target.startswith(ENGINE_PREFIX):
            continue
        package = target.removeprefix(ENGINE_PREFIX)
        if package not in PUBLIC_SEAMS:
            result.add((edge.get("from", ""), package))
    return result


def check(graph: dict[str, Any]) -> list[str]:
    actual = legacy_imports(graph)
    problems = []
    for package, target in sorted(actual - EXPECTED_LEGACY_IMPORTS):
        problems.append(f"unexpected Cloud production import: {package} -> Engine {target}")
    for package, target in sorted(EXPECTED_LEGACY_IMPORTS - actual):
        problems.append(f"legacy import allowlist is stale: {package} no longer imports Engine {target}")
    return problems


def main() -> int:
    graph_path = Path(__file__).resolve().parents[1] / "docs/architecture/import-graph.json"
    try:
        graph = json.loads(graph_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        print(f"cannot read complete architecture import graph {graph_path}: {exc}", file=sys.stderr)
        return 2
    problems = check(graph)
    if problems:
        print("Cloud-to-Engine import boundary check failed:", file=sys.stderr)
        for problem in problems:
            print(f"- {problem}", file=sys.stderr)
        return 1
    print("Normal Cloud build has no Engine implementation imports.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
