# Why Tree — test plan (token-frugal)

Four tiers, ordered cheapest-first. **T1 + T2 cost zero LLM tokens** and validate everything except the live agent intelligence. T3/T4 cost tokens and run only on explicit request.

| Tier | What it proves | LLM cost | Run |
|------|----------------|----------|-----|
| **T1 · Render/contract** | The visualization renders correctly from arbitrary tree-JSON: escaping, deep nesting, refuted/downgraded roles, constraint spotlight, empty panels, long text, the convergence map. | **0 tokens** | `python tests/build_fixtures.py` → open `tests/out/*.html` |
| **T2 · Plumbing** | The workflow orchestration wiring (fan-out → dedup → deepen → converge) flows data correctly and emits **schema-valid** tree-JSON that the viz accepts. Agents are stubbed with canned data. | **0 tokens** | `node tests/plumbing.test.mjs` |
| **T3 · Smoke workflow** | The real Workflow engine actually fans out and returns a valid tree, end-to-end. Bounded: 3 reasoning-only lenses, 1-level deepen, no refute, 1 converge ≈ **4 small agents**. | ~low (4 agents, no web) | run `tests/smoke-workflow.js` via the Workflow tool |
| **T4 · Quality eval** | The tree's *content* is good: it grades nodes honestly, converges to the *right* constraint on a known-answer problem, and the census is calibrated. | high (Deep pass) | manual, on a problem with a known root cause |

## What each tier does NOT cover
- T1 says nothing about whether the *analysis* is good — only that any valid tree displays correctly.
- T2 stubs the agents — it proves the pipes connect and the schema holds, not that real agents fill them well.
- T3 proves the engine runs but at smoke depth on a toy problem — not diagnostic quality.
- Only T4 judges quality, and it's the expensive one — keep it for when the methodology itself is in question, on a problem whose real constraint you already know (so you can score the skill's answer).

## Fixtures (T1)
- `fixtures/minimal.json` — sparsest valid tree (missing optional fields, one root, empty negatives/tests). Tests graceful degradation.
- `fixtures/edge.json` — escaping (`<`, `&`, quotes, emoji), 3-level nesting, REFUTED + DOWNGRADED + CONSTRAINT-in-stage spotlight, very long text, multi-root convergence. The comprehensive stress test + the best single viz example to eyeball.
- The default `assets/sample-tree.html` is the realistic "rich" example.

## Pass criteria
- **T1:** every fixture opens with no console errors; all special chars display literally (no broken HTML / no XSS execution); refuted nodes strike through; the constraint glows; the convergence lines draw; empty panels don't crash.
- **T2:** `node tests/plumbing.test.mjs` prints `PASS` — dedup reduced branch count, every node has kind+grade, exactly one root is the constraint, and the output validates against the schema.
