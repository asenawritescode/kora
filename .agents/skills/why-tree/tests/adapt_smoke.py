#!/usr/bin/env python3
"""Zero-token adapter: maps a live smoke-workflow result (tests/smoke-raw.json) into the
visualization's tree-JSON schema (tests/out/smoke-tree.json), so a tree produced by the
REAL engine can be rendered. (The durable fix is the tightened converge schema in
smoke-workflow.js / workflow-template.md, which makes this adapter unnecessary going forward.)
  run:  python3 tests/adapt_smoke.py && python3 tests/build_fixtures.py
"""
import json
from pathlib import Path

HERE = Path(__file__).resolve().parent
raw = json.loads((HERE / "smoke-raw.json").read_text())
if "result" in raw and "apex" not in raw:   # unwrap workflow output envelope {summary,logs,result}
    raw = raw["result"]

GRADE = {"A": "Strong", "B": "Mod", "C": "Weak"}
KIND = {"HYPOTHESIS": "HYPOTHESIS", "INFERENCE": "INFERENCE", "OBSERVATION": "INSTANCE", "FACT": "MEASURED"}

def kind_from_absorbs(absorbs):
    txt = " ".join(absorbs or []).upper()
    for k in ("FACT", "OBSERVATION", "INFERENCE", "HYPOTHESIS"):
        if k in txt:
            return KIND[k]
    return "HYPOTHESIS"

roots_raw = raw.get("roots", [])
# one node id per root's stage; the A-grade root is THE constraint
node_id = {}
stages = []
for i, r in enumerate(roots_raw):
    nid = "N%d" % i
    node_id[r["stage"]] = nid
    is_con = r.get("grade") == "A"
    role = "CONSTRAINT" if is_con else ("ROOT" if r.get("grade") == "A" else "LEVER")
    stages.append({
        "id": "S%d" % i,
        "title": "%s" % r["stage"].replace("-", " ").title(),
        "crosscut": is_con,
        "nodes": [{
            "id": nid, "text": r["root"],
            "kind": kind_from_absorbs(r.get("absorbs")),
            "grade": GRADE.get(r.get("grade"), "Weak"),
            "role": role,
            "cite": (r.get("note", "") + "  ·  reasoning-only (smoke, no web)").strip(),
        }],
    })

con_root_idx = next((i for i, r in enumerate(roots_raw) if r.get("grade") == "A"), 0)
roots = []
for i, r in enumerate(roots_raw):
    roots.append({
        "id": "R%d" % i, "title": r["root"],
        "from": [node_id[r["stage"]]],
        "action": r.get("note", "").split(".")[0],
        "isConstraint": (i == con_root_idx),
    })

c = raw.get("constraint", {})
constraint = {
    "rootId": "R%d" % con_root_idx, "type": "behavioral",
    "statement": c.get("why", ""),
    "unlocated": "Leverage: " + c.get("leverage", ""),
}

bk = raw.get("census", {}).get("by_kind", {})
total = sum(bk.values()) or 1
census = {
    "measured": bk.get("FACT", 0) / total,
    "instance": bk.get("OBSERVATION", 0) / total,
    "inference": bk.get("INFERENCE", 0) / total,
    "hypothesis": bk.get("HYPOTHESIS", 0) / total,
    "claim": 0, "external": 0,
    "bottomLine": raw.get("census", {}).get("bottom_line", ""),
}

tests = [{"test": t["test"], "decides": t.get("targets", ""), "rank": "#%d" % (i + 1)}
         for i, t in enumerate(raw.get("tests", []))]

tree = {
    "apex": raw["apex"],
    "provenance": {"method": "Goldratt CRT", "depth": "smoke (live engine)", "agents": 4, "date": "smoke"},
    "corrections": ["Live ~4-agent smoke run, reasoning-only / no web — adapted from the engine's raw output to the viz schema."],
    "stages": stages, "roots": roots, "constraint": constraint,
    "negatives": raw.get("negatives", []), "tests": tests, "census": census,
}

(HERE / "out").mkdir(exist_ok=True)
(HERE / "out" / "smoke-tree.json").write_text(json.dumps(tree, indent=2))
print("wrote tests/out/smoke-tree.json  (constraint = root R%d)" % con_root_idx)
