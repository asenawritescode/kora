#!/usr/bin/env python3
"""Gate dead code with a baseline ratchet.

Runs `deadcode ./...` (golang.org/x/tools/cmd/deadcode), which lists functions that cannot be reached
from the kora binary or any cmd/ tool, and compares the result with .lint-baseline/deadcode.txt.

  * a function that is newly unreachable fails the check: delete it, or make it used;
  * a function that is no longer listed (deleted, or now used) is fine and can be dropped from the
    baseline with `make deadcode-baseline`.

Code that is deliberately kept although the binary does not reach it (public API for another
repository, helpers shared by integration tests) must be added to the baseline with --allow-new, so the
exception is visible in review. Entries are keyed by file and function name, not line number.
"""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
BASELINE = ROOT / ".lint-baseline" / "deadcode.txt"
LINE = re.compile(r"^(?P<file>[^:]+):\d+:\d+: unreachable func: (?P<name>.+)$")


def measure() -> set[str]:
    result = subprocess.run(["deadcode", "./..."], cwd=ROOT, capture_output=True, text=True, check=False)
    if result.returncode != 0:
        sys.exit(f"deadcode failed ({result.returncode}):\n{result.stderr[-2000:]}")
    found = set()
    for line in result.stdout.splitlines():
        match = LINE.match(line.strip())
        if match:
            found.add(f"{match['file']}::{match['name']}")
    return found


def load_baseline() -> set[str]:
    if not BASELINE.exists():
        return set()
    return {line.strip() for line in BASELINE.read_text(encoding="utf-8").splitlines()
            if line.strip() and not line.startswith("#")}


def write_baseline(entries: set[str]) -> None:
    BASELINE.parent.mkdir(parents=True, exist_ok=True)
    header = ("# Functions unreachable from the kora binary and cmd/ tools (deadcode ./...).\n"
              "# This list may only shrink; see scripts/deadcode-ratchet.py.\n")
    BASELINE.write_text(header + "\n".join(sorted(entries)) + "\n", encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--init", action="store_true", help="create the baseline from the current state")
    parser.add_argument("--update", action="store_true", help="drop entries that are no longer dead")
    parser.add_argument("--allow-new", action="store_true", help="with --update, also accept newly dead code")
    args = parser.parse_args()

    current = measure()
    if args.init:
        write_baseline(current)
        print(f"baseline written: {len(current)} unreachable functions")
        return 0

    baseline = load_baseline()
    new = sorted(current - baseline)
    gone = sorted(baseline - current)

    if args.update:
        if new and not args.allow_new:
            print("refusing to add newly dead functions:\n" + "\n".join(f"  {k}" for k in new), file=sys.stderr)
            return 1
        write_baseline(current if args.allow_new else current & baseline)
        print(f"baseline updated: {len(current & baseline) if not args.allow_new else len(current)} entries "
              f"({len(gone)} removed)")
        return 0

    if new:
        print("Dead code ratchet failed. These functions are not reachable from the binary or any cmd/ tool:",
              file=sys.stderr)
        print("\n".join(f"  {key}" for key in new), file=sys.stderr)
        print("\nDelete them, or call them from production code. If one must stay (public API for another "
              "repository, a helper shared by integration tests), add it with "
              "`python3 scripts/deadcode-ratchet.py --update --allow-new` and explain why in the PR.",
              file=sys.stderr)
        return 1
    print(f"deadcode ratchet ok: {len(current)} unreachable functions, none new")
    if gone:
        print(f"{len(gone)} no longer dead; run `make deadcode-baseline` to shrink the baseline:")
        print("\n".join(f"  {key}" for key in gone[:20]))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
