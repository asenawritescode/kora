#!/usr/bin/env python3
"""Generate a complete, deterministic Go import graph for Kora repositories.

Each module is enumerated with ordinary `go list -json ./...`; failures stop the
generator. Test imports are included, and package directories outside the
module root (vendored/generated dependencies) are excluded from graph edges.
"""

from __future__ import annotations

import argparse
import json
import pathlib
import subprocess
import sys


ROOT = pathlib.Path(__file__).resolve().parents[1]
DEFAULT_MODULES = [
    ("engine", ROOT, "github.com/asenawritescode/kora"),
    ("engine-sdk", ROOT / "sdk", "github.com/asenawritescode/kora/sdk"),
    ("cloud", ROOT.parent / "kora-cloud", "github.com/asenawritescode/kora-cloud"),
]


def packages_for(name: str, path: pathlib.Path, module_path: str) -> list[dict]:
    if not path.is_dir():
        raise RuntimeError(f"module {name} directory does not exist: {path}")
    completed = subprocess.run(
        ["go", "list", "-json", "./..."],
        cwd=path,
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    if completed.returncode:
        raise RuntimeError(
            f"go list failed for {name} ({path}):\n{completed.stderr.strip()}"
        )
    decoder = json.JSONDecoder()
    data = completed.stdout
    packages = []
    offset = 0
    while offset < len(data):
        while offset < len(data) and data[offset].isspace():
            offset += 1
        if offset == len(data):
            break
        package, offset = decoder.raw_decode(data, offset)
        if "Error" in package or "Incomplete" in package:
            raise RuntimeError(f"go list returned incomplete package for {name}: {package}")
        packages.append(package)
    if not packages:
        raise RuntimeError(f"go list returned no packages for {name}")
    return packages


def is_first_party(package: dict, module_path: str, module_root: pathlib.Path) -> bool:
    if not package.get("ImportPath", "").startswith(module_path):
        return False
    directory = pathlib.Path(package.get("Dir", "")).resolve()
    try:
        relative = directory.relative_to(module_root.resolve())
    except ValueError:
        return False
    # Go can discover third-party sample packages beneath a frontend's
    # node_modules. Those sources are dependencies, not Engine packages.
    excluded = {"vendor", "node_modules"}
    return not any(part in excluded for part in relative.parts)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true", help="fail if generated graph differs")
    parser.add_argument(
        "--output",
        type=pathlib.Path,
        default=ROOT / "docs/architecture/import-graph.json",
    )
    args = parser.parse_args()

    packages = []
    module_packages: dict[str, set[str]] = {}
    for name, path, module_path in DEFAULT_MODULES:
        listed = packages_for(name, path, module_path)
        internal = {
            package["ImportPath"]
            for package in listed
            if is_first_party(package, module_path, path)
        }
        module_packages[name] = internal
        packages.extend((name, path, package) for package in listed if package["ImportPath"] in internal)

    edges = []
    nodes = {name: imports for name, imports in module_packages.items()}
    all_nodes = {node for imports in nodes.values() for node in imports}
    for module, path, package in packages:
        source = package["ImportPath"]
        for import_kind in ("Imports", "TestImports", "XTestImports"):
            for target in package.get(import_kind, []):
                if target in all_nodes:
                    edges.append(
                        {
                            "from": source,
                            "from_module": module,
                            "kind": import_kind.removesuffix("Imports").lower() or "production",
                            "to": target,
                        }
                    )
    edges.sort(key=lambda edge: (edge["from_module"], edge["from"], edge["kind"], edge["to"]))
    result = {
        "format": 1,
        "generator": "scripts/architecture-import-graph.py",
        "modules": {
            name: {"package_count": len(imports), "module_path": next(
                module_path for module_name, _, module_path in DEFAULT_MODULES if module_name == name
            )}
            for name, imports in sorted(nodes.items())
        },
        "edges": edges,
    }
    rendered = json.dumps(result, indent=2, sort_keys=True) + "\n"
    if args.check:
        try:
            current = args.output.read_text(encoding="utf-8")
        except FileNotFoundError:
            current = ""
        if current != rendered:
            print(f"architecture import graph is stale: {args.output}", file=sys.stderr)
            return 1
        print(f"architecture import graph is current: {args.output}")
        return 0
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(rendered, encoding="utf-8")
    print(f"wrote {args.output} ({len(edges)} internal import edges)")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, RuntimeError, json.JSONDecodeError) as error:
        print(error, file=sys.stderr)
        raise SystemExit(2)
