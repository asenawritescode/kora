#!/usr/bin/env python3
"""Validate and render a concise evidence ledger from JSON."""
import argparse
import json
from pathlib import Path


def validate(data):
    required = ("issue", "base_revision", "integration_branch", "pr", "head_revision", "criteria", "checks", "review_status", "disposition")
    missing = [key for key in required if key not in data]
    if missing:
        raise ValueError("missing fields: " + ", ".join(missing))
    for criterion in data["criteria"]:
        if criterion.get("status") not in {"PASS", "FAIL", "UNVERIFIED"} or not criterion.get("id"):
            raise ValueError("each criterion needs id and PASS/FAIL/UNVERIFIED status")
    for check in data["checks"]:
        if check.get("outcome") not in {"PASS", "FAIL", "SKIP", "UNAVAILABLE"}:
            raise ValueError("check outcome must be PASS/FAIL/SKIP/UNAVAILABLE")
    if data.get("disposition") == "DONE":
        if not data.get("merge_sha"):
            raise ValueError("DONE requires an identifiable merge_sha")
        if any(c["status"] != "PASS" for c in data["criteria"]):
            raise ValueError("DONE requires PASS evidence for every criterion")
        if any(c["outcome"] != "PASS" for c in data["checks"]):
            raise ValueError("DONE requires every required check to PASS")
        if data.get("review_status") not in {"clear", "resolved", "none"}:
            raise ValueError("DONE requires no outstanding blocking review thread")
        if not data.get("evidence_url"):
            raise ValueError("DONE requires recorded evidence URL")
    return data


def feedback_class(label):
    """Return handling for a normalized feedback class."""
    if label in {"correctness defect", "missing acceptance criterion", "test gap"}:
        return "in_scope"
    if label in {"enhancement/new requirement", "unrelated suggestion"}:
        return "out_of_scope"
    if label == "scope clarification":
        return "decision_required"
    raise ValueError("unknown feedback class")


def render(data):
    validate(data)
    lines = [f"Evidence: {data['issue']}", f"Base: {data['base_revision']} ({data['integration_branch']})",
             f"PR: {data['pr']} @ {data['head_revision']}"]
    if data.get("merge_sha"):
        lines.append(f"Merge: {data['merge_sha']}")
    lines.append("Criteria:")
    lines.extend(f"- {c['id']}: {c['status']} — {c.get('evidence', 'evidence not supplied')}" for c in data["criteria"])
    lines.append("Checks:")
    lines.extend(f"- {c.get('command', 'check')}: {c['outcome']}" + (f" ({c.get('url')})" if c.get("url") else "") for c in data["checks"])
    lines.extend([f"Review: {data['review_status']}", f"Disposition: {data['disposition']}"])
    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("ledger", help="JSON evidence ledger")
    args = ap.parse_args()
    try:
        print(render(json.loads(Path(args.ledger).read_text())))
    except (ValueError, json.JSONDecodeError) as exc:
        ap.error(str(exc))


if __name__ == "__main__":
    main()
