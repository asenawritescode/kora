#!/usr/bin/env python3
"""Gate the number of parameters a function takes, with a baseline that can only shrink.

`go run ./cmd/param-count` prints "dir::Name count" for every function over the limit (5, the receiver
not counted). The baseline maps each existing offender to its count:

  * a function that is not in the baseline fails the check (a new offender);
  * a listed function with more parameters than its baseline fails (it got worse);
  * a function that dropped to fewer parameters, or is gone, is fine, and `--update` records it.

`--init` creates the baseline; `--update` lowers it and never adds. Used by `make params-ratchet`.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
BASELINE = ROOT / ".lint-baseline" / "params.json"
COMMENT = "Functions over the parameter limit (cmd/param-count). This file may only shrink; see scripts/params-ratchet.py."


def measure() -> dict[str, int]:
    result = subprocess.run(["go", "run", "./cmd/param-count"], cwd=ROOT, capture_output=True, text=True, check=False)
    if result.returncode != 0:
        sys.exit(f"param-count failed ({result.returncode}):\n{result.stderr[-2000:]}")
    found: dict[str, int] = {}
    for line in result.stdout.splitlines():
        key, _, count = line.strip().rpartition(" ")
        if key:
            found[key] = int(count)
    return found


def load() -> dict[str, int]:
    if not BASELINE.exists():
        return {}
    return json.loads(BASELINE.read_text(encoding="utf-8")).get("functions", {})


def save(functions: dict[str, int]) -> None:
    BASELINE.parent.mkdir(parents=True, exist_ok=True)
    lines = [f'  {json.dumps(k)}: {functions[k]}' for k in sorted(functions)]
    body = ",\n".join(lines)
    BASELINE.write_text(f'{{\n "comment": {json.dumps(COMMENT)},\n "functions": {{\n{body}\n }}\n}}\n', encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--init", action="store_true")
    parser.add_argument("--update", action="store_true")
    args = parser.parse_args()

    current = measure()
    if args.init:
        save(current)
        print(f"params baseline written ({len(current)} functions)")
        return 0

    baseline = load()
    new = {k: n for k, n in current.items() if k not in baseline}
    worse = {k: (baseline[k], n) for k, n in current.items() if k in baseline and n > baseline[k]}
    if args.update:
        if new or worse:
            print("refusing to raise the baseline; fix the offenders first", file=sys.stderr)
            return 1
        save(current)
        print(f"params baseline updated ({len(current)} functions)")
        return 0

    if new or worse:
        print("Parameter-count ratchet failed. Take ctx, the required arguments and one options struct:", file=sys.stderr)
        for key, count in sorted(new.items()):
            print(f"  NEW   {key}: {count} parameters", file=sys.stderr)
        for key, (before, count) in sorted(worse.items()):
            print(f"  WORSE {key}: {before} -> {count} parameters", file=sys.stderr)
        return 1
    better = {k for k, n in current.items() if n < baseline.get(k, n)} | (set(baseline) - set(current))
    print(f"params ratchet ok: {len(current)} functions over the limit, none new or worse")
    if better:
        print(f"{len(better)} improved; run `make params-baseline` to lower the baseline")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
