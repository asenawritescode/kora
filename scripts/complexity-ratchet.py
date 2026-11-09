#!/usr/bin/env python3
"""Gate function size and complexity with a baseline ratchet.

Runs golangci-lint with .golangci-complexity.yml and compares every function that is over a limit
(cognitive complexity, cyclomatic complexity, length, statement count) with .lint-baseline/complexity.json.

  * a function that is newly over a limit fails the check;
  * a function that is already over a limit and gets worse fails the check;
  * a function that improves or disappears is fine, and the baseline can then be lowered.

The baseline only ever shrinks. `make complexity-baseline` lowers it; raising it needs the explicit
--allow-increase flag and shows up in review as a baseline diff.

Functions are keyed by package directory and name, never by line number, so edits elsewhere in a file
do not disturb the baseline.
"""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
BASELINE = ROOT / ".lint-baseline" / "complexity.json"
CONFIG = ROOT / ".golangci-complexity.yml"

PATTERNS = (
    ("cognitive", "gocognit", re.compile(r"cognitive complexity (\d+) of func `([^`]+)`")),
    ("cyclomatic", "gocyclo", re.compile(r"cyclomatic complexity (\d+) of func `([^`]+)`")),
    ("lines", "funlen", re.compile(r"Function '([^']+)' is too long \((\d+) > \d+\)")),
    ("statements", "funlen", re.compile(r"Function '([^']+)' has too many statements \((\d+) > \d+\)")),
)


def measure() -> dict[str, dict[str, int]]:
    """Return {"pkg/dir::func": {metric: value}} for every function over a limit."""
    result = subprocess.run(
        ["golangci-lint", "run", "--config", str(CONFIG), "--out-format", "json", "./..."],
        cwd=ROOT, capture_output=True, text=True, check=False,
    )
    start = result.stdout.find('{"Issues"')
    if start < 0:
        if result.returncode not in (0, 1):
            sys.exit(f"golangci-lint failed ({result.returncode}):\n{result.stderr[-2000:]}")
        return {}
    issues = json.JSONDecoder().raw_decode(result.stdout[start:])[0].get("Issues") or []
    found: dict[str, dict[str, int]] = {}
    for issue in issues:
        text, linter = issue["Text"], issue["FromLinter"]
        filename = issue["Pos"]["Filename"]
        directory = filename.rsplit("/", 1)[0] if "/" in filename else "."
        for metric, name, pattern in PATTERNS:
            if linter != name:
                continue
            match = pattern.search(text)
            if not match:
                continue
            # gocognit/gocyclo print "<n> of func <name>"; funlen prints "<name> ... (<n> > limit)".
            if name == "funlen":
                function, value = match.group(1), int(match.group(2))
            else:
                value, function = int(match.group(1)), match.group(2)
            found.setdefault(f"{directory}::{function}", {})[metric] = value
    return found


def load_baseline() -> dict[str, dict[str, int]]:
    if not BASELINE.exists():
        return {}
    return json.loads(BASELINE.read_text(encoding="utf-8")).get("functions", {})


def write_baseline(functions: dict[str, dict[str, int]]) -> None:
    BASELINE.parent.mkdir(parents=True, exist_ok=True)
    document = {
        "comment": "Functions over the limits in .golangci-complexity.yml. This file may only shrink; "
                   "see scripts/complexity-ratchet.py.",
        "functions": dict(sorted(functions.items())),
    }
    BASELINE.write_text(json.dumps(document, indent=1, sort_keys=False) + "\n", encoding="utf-8")


def compare(current: dict[str, dict[str, int]], baseline: dict[str, dict[str, int]]):
    violations: list[str] = []
    improvements: list[str] = []
    for key, metrics in sorted(current.items()):
        base = baseline.get(key)
        for metric, value in sorted(metrics.items()):
            if base is None or metric not in base:
                violations.append(f"  NEW   {key}: {metric} {value} is over the limit")
            elif value > base[metric]:
                violations.append(f"  WORSE {key}: {metric} {base[metric]} -> {value}")
            elif value < base[metric]:
                improvements.append(f"  {key}: {metric} {base[metric]} -> {value}")
    for key, base in sorted(baseline.items()):
        if key not in current:
            improvements.append(f"  {key}: now within every limit")
    return violations, improvements


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--update", action="store_true", help="write the baseline (only lowers it)")
    parser.add_argument("--init", action="store_true", help="create the baseline from the current state")
    parser.add_argument("--allow-increase", action="store_true", help="with --update, accept worse functions")
    args = parser.parse_args()

    current = measure()
    if args.init:
        write_baseline(current)
        print(f"baseline written: {len(current)} functions over a limit")
        return 0

    baseline = load_baseline()
    violations, improvements = compare(current, baseline)

    if args.update:
        if violations and not args.allow_increase:
            print("refusing to raise the baseline:\n" + "\n".join(violations), file=sys.stderr)
            return 1
        write_baseline(current if args.allow_increase else {k: v for k, v in current.items() if k in baseline})
        print(f"baseline updated: {len(current)} functions over a limit ({len(improvements)} improvements)")
        return 0

    if violations:
        print("Function complexity ratchet failed. Split the function instead of growing it:", file=sys.stderr)
        print("\n".join(violations), file=sys.stderr)
        print("\nCognitive/cyclomatic complexity and length limits are in .golangci-complexity.yml. "
              "Extract cohesive steps into named functions with their own tests.", file=sys.stderr)
        return 1
    print(f"complexity ratchet ok: {len(current)} functions over a limit, none new or worse")
    if improvements:
        print(f"{len(improvements)} improved; run `make complexity-baseline` to lower the baseline:")
        print("\n".join(improvements[:20]))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
