#!/usr/bin/env python3
"""Read-only git safety report for a proposed execution."""
import argparse
import json
import subprocess


def git(*args):
    p = subprocess.run(["git", *args], text=True, capture_output=True)
    return p.returncode, p.stdout.strip(), p.stderr.strip()


def inspect(target=None):
    rc, branch, err = git("branch", "--show-current")
    if rc:
        return {"status": "BLOCKED", "reason": err or "not a git worktree"}
    _, porcelain, _ = git("status", "--porcelain=v1", "--untracked-files=all")
    _, worktrees, _ = git("worktree", "list", "--porcelain")
    _, head, _ = git("rev-parse", "HEAD")
    dirty = bool(porcelain)
    base = None
    if target:
        b_rc, base, b_err = git("rev-parse", "--verify", f"refs/heads/{target}")
        if b_rc:
            return {"status": "BLOCKED", "reason": f"target branch unavailable: {b_err}"}
    status = "BLOCKED" if dirty else "SAFE_TO_REVIEW"
    return {"status": status, "branch": branch, "target": target, "head": head,
            "dirty": dirty, "changes": porcelain.splitlines(),
            "worktree_count": worktrees.count("worktree "),
            "reason": "preserve local changes before execution" if dirty else "inspect matching PRs and task WIP before editing"}


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--target", help="target integration branch to verify locally")
    args = ap.parse_args()
    print(json.dumps(inspect(args.target), indent=2))


if __name__ == "__main__":
    main()
