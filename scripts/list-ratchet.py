#!/usr/bin/env python3
"""Gate a list of known offenders with a baseline that can only shrink.

Runs a command that prints one offender per line and compares the lines with a baseline file:

  * a line that is not in the baseline fails the check (a new offender);
  * a line that is in the baseline but no longer printed is fine, and `--update` removes it.

`--update --allow-new` also accepts new offenders, so the exception shows up in review as a baseline diff.
`--init` creates the baseline. Used by `make primitive-funcs-ratchet`; see CLAUDE.md for what each list means.
"""

from __future__ import annotations

import argparse
import shlex
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def measure(command: str) -> set[str]:
    result = subprocess.run(shlex.split(command), cwd=ROOT, capture_output=True, text=True, check=False)
    if result.returncode != 0:
        sys.exit(f"`{command}` failed ({result.returncode}):\n{result.stderr[-2000:]}")
    return {line.strip() for line in result.stdout.splitlines() if line.strip()}


def load(path: Path) -> set[str]:
    if not path.exists():
        return set()
    return {line.strip() for line in path.read_text(encoding="utf-8").splitlines()
            if line.strip() and not line.startswith("#")}


def write(path: Path, header: str, entries: set[str]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(header + "\n".join(sorted(entries)) + "\n", encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--name", required=True, help="short name used in messages")
    parser.add_argument("--cmd", required=True, help="command that prints one offender per line")
    parser.add_argument("--baseline", required=True, help="baseline file, relative to the repository root")
    parser.add_argument("--hint", default="", help="how to fix a new offender")
    parser.add_argument("--init", action="store_true")
    parser.add_argument("--update", action="store_true")
    parser.add_argument("--allow-new", action="store_true")
    args = parser.parse_args()

    baseline_path = ROOT / args.baseline
    header = f"# {args.name}: known offenders. This list may only shrink; see scripts/list-ratchet.py.\n"
    current = measure(args.cmd)
    if args.init:
        write(baseline_path, header, current)
        print(f"{args.name}: baseline written ({len(current)} entries)")
        return 0

    baseline = load(baseline_path)
    new, gone = sorted(current - baseline), sorted(baseline - current)
    if args.update:
        if new and not args.allow_new:
            print(f"{args.name}: refusing to add new offenders:\n" + "\n".join(f"  {k}" for k in new), file=sys.stderr)
            return 1
        write(baseline_path, header, current if args.allow_new else current & baseline)
        print(f"{args.name}: baseline updated ({len(gone)} removed)")
        return 0

    if new:
        print(f"{args.name} ratchet failed. New offenders:", file=sys.stderr)
        print("\n".join(f"  {k}" for k in new), file=sys.stderr)
        if args.hint:
            print("\n" + args.hint, file=sys.stderr)
        return 1
    print(f"{args.name} ratchet ok: {len(current)} known, none new")
    if gone:
        print(f"{len(gone)} fixed; run the matching -baseline target to shrink the baseline")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
