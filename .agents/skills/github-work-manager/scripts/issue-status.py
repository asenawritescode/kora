#!/usr/bin/env python3
"""Fail-closed READY issue selection from a GitHub-derived JSON snapshot."""
import argparse
import json
from datetime import datetime
from pathlib import Path


def select(snapshot):
    if not snapshot.get("metadata_available", False):
        return {"status": "UNKNOWN", "reason": "GitHub metadata unavailable", "selected": None}
    limit = int(snapshot.get("global_wip_limit", 1))
    active = int(snapshot.get("active_tasks", 0))
    if active >= limit:
        return {"status": "NO_SELECTION", "reason": "global WIP limit is full", "selected": None}
    candidates = []
    for issue in snapshot.get("issues", []):
        if issue.get("state") != "open" or issue.get("kind") != "task":
            continue
        if issue.get("repo_active") or issue.get("active_pr"):
            continue
        if not issue.get("criteria_sufficient") or issue.get("cross_repo_scope"):
            continue
        if issue.get("oversized"):
            continue
        if issue.get("already_implemented") or not issue.get("prerequisites_integrated"):
            continue
        deps = issue.get("dependencies")
        if deps is None:  # None means the relationship query failed, not no dependencies.
            continue
        if any(d.get("state") != "done" or not d.get("integrated") for d in deps):
            continue
        priority = issue.get("priority")
        if priority is None:
            continue
        try:
            created = datetime.fromisoformat(issue["created_at"].replace("Z", "+00:00"))
        except (KeyError, ValueError):
            continue
        candidates.append((int(priority), int(issue.get("unlocks", 0)), created, issue))
    if not candidates:
        return {"status": "NO_SELECTION", "reason": "no issue satisfies READY rules", "selected": None}
    # Lower numeric priority is higher priority; then unlock count descending, then oldest.
    _, _, _, issue = min(candidates, key=lambda row: (row[0], -row[1], row[2]))
    return {"status": "READY", "reason": "highest priority, unlock value, then age", "selected": issue}


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("snapshot", help="JSON snapshot gathered from gh/API; no network calls are made")
    args = ap.parse_args()
    print(json.dumps(select(json.loads(Path(args.snapshot).read_text())), indent=2))


if __name__ == "__main__":
    main()
