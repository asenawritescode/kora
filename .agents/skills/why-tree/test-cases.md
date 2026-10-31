# Why Tree — validation cases

Non-confidential cases to sanity-check the skill. The `tests/` folder has a zero-token plumbing harness (schema/render checks) and fixtures; the cases below are end-to-end behavior checks.

## Case 1 — "New Year's resolutions fail by February" (no private data; pure reasoning)

**Apex (as a statement):** *"Most New Year's resolutions are abandoned by February."*

A correct tree should:
- Fan out across lenses (psychology, mechanics, incentives, measurement) before deepening any one.
- Refute the tempting-but-shallow "they're too hard" branch (modest resolutions also fail → difficulty isn't the constraint).
- Converge to ONE constraint that is a **policy/behavior**, not a resource — e.g. "resolutions are framed as outcome goals with no concrete cue-action-reward loop" — and drill it to bedrock.
- Census: heavy on EXTERNAL/INFERENCE (it's a reasoning problem) → honestly flagged as such.

Fixture: `tests/smoke-raw.json`.

## Case 2 — "Personal OKRs abandoned by week 3" (tests schema + render)

**Apex:** *"Most personal OKRs get abandoned by week 3."*

Checks the tree-JSON contract renders: nodes carry kind/grade/cite; a node with no citation is flagged; the constraint root carries a nested `children[]` chain; the census renders. Fixture: `tests/fixtures/minimal.json` (+ `edge.json` for the awkward shapes).

## Case 3 — Idea Audit: "an onboarding-knowledge product" (field-validated 2026-07, anonymized)

**Invocation (an idea, not a problem):** *"I want to build a product that ingests meeting recordings + docs and generates an onboarding program with knowledge search — would it work?"*

**Extracted apex (the problem the idea presupposes):** *"Contract engineers take 3-5 months to onboard, with no earlier signal on fit or progress."*

A correct Idea Audit should:
- Extract + confirm the apex, then keep the idea OUT of every lens/deepen/refute prompt (the quarantine).
- Converge on a **policy/ownership** constraint — no owner of the onboarding process; the wrong party (the client, not the contractor) runs it — not a knowledge-tooling constraint.
- Reframe the decisive question: not *"how to navigate 4 months of knowledge"* but *"how to know in 2 weeks whether A is onboarding better than B."*
- Fit-check verdict: **misses-constraint** — the idea shortens knowledge navigation while the constraint is ownership; the policy-tool mismatch is flagged explicitly.
- Grade the kill: absorbed-pain = `chronic-absorbed` (orgs with 15-20-year-old systems budgeted around slow onboarding long ago → low-WTP **hypothesis** with a pre-sell / budget-line test attached), never an ungraded pronouncement.
- Emit the segment fork (dies: client-owned onboarding of 1-2 contractors, absorbed pain · may live: 100-person cohorts, acute trigger, named owner) and the redirect (an early-signal instrument owned by the contractor).

The real field run reached exactly this shape by hand — two conversational steps after a finished tree; the mode codifies it (including the post-hoc `idea-check` entry).

Fixture: `tests/fixtures/edge.json` (its `idea`/`ideaFit` block — covers escaping, the verify-first badge via a weak `restsOn`, contested, and orphaned).

## What "good" looks like (the rubric)

- **Exactly one** system constraint (not five co-equal roots).
- Every node graded; no ungraded assertions.
- At least one load-bearing branch shown as **REFUTED/DOWNGRADED with the killing evidence** (a tree with zero refuted branches usually means the refute pass didn't really run).
- The constraint branch is a **multi-level chain to bedrock**, not a one-line root.
- An **honest census** — if the decisive nodes are HYPOTHESIS, the headline says "map, not verdict," and the #1 cheapest test targets exactly those nodes.
- The apex is a **declarative statement**, never a question, never a smuggled solution.

## Anti-cases (the skill should refuse / reframe)

- Apex smuggling a solution ("why don't we have feature Y") → reframe to the underlying problem — **but an idea brought for evaluation routes to Idea Audit** (extract the presupposed problem, quarantine the idea, fit-check after converge), it is not refused.
- A pure "how many X?" data-pull → reframe into the gap/decision; counts are evidence, not the apex.
- A clear-cut problem one careful pass would nail → say so and answer directly; don't spin up the multi-agent workflow.
