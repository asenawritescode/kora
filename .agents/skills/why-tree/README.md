# Why Tree

A heavyweight, **agentic** Goldratt Current-Reality Tree. Point it at a hard, contested problem and it fans **tens of AI agents** (scout gate ~5 · Standard 22-40 · Deep 90-115 observed) across the problem's branch-space, **grades every node by the kind of evidence behind it** (MEASURED / INFERENCE / CLAIM / HYPOTHESIS …), **tries to refute its own load-bearing branches**, and **converges on the ONE system constraint** — plus the negative branches (fixes that backfire) and the single cheapest test that would fork-decide. Output: a self-contained **interactive HTML tree** (opening with a plain-prose narrative memo; a growth-*target* apex additionally gets a sized **lever portfolio**) + a one-page **decision doc**.

This is the **"build the evidence and find the wall"** move. Its siblings: `/constraint-finder` is the lightweight coach that returns Goldratt's **5 Focusing Steps** once you already know the constraint; `/triz-dissolve` is the **"move the wall"** move. The natural chain is **why-tree → constraint-finder**: this skill *locates and evidences* the constraint on a messy problem; constraint-finder tells you what to *do* about it.

It also runs an **Idea Audit** (CRT→FRT: a solution is an *injection*, judged against the tree, never against the symptom). Bring a product idea — "I want to build X, would it work?" — and the skill inverts it: diagnoses the problem the idea *presupposes* (with the idea quarantined from the swarm, so the tree isn't built to flatter the pitch), then fit-checks the idea against the located constraint. Does it dissolve the constraint or just relieve a symptom? Is the constraint a policy an ownership-neutral tool can't touch? Has the buyer *absorbed* the pain as a cost of doing business (a graded low-WTP hypothesis with its pre-sell test attached — never an ungraded kill)? Plus the **segment fork** — where the idea dies vs where it might live (an n=1 diagnosis is ICP selection, not a market verdict) — and the **redirect**: what *would* dissolve the constraint. Scope is deliberately narrow: the fit of one idea to one diagnosed system; market sizing and competition are out of scope. Also works post-hoc on a finished tree (`why-tree idea-check`, +2 agents).

> **5-Whys grown up.** Naive root-causing follows a single thread, stops at the first plausible cause, and cites nothing. A Why Tree searches the *whole* branch-space in parallel, grades every node, refutes before it trusts, and forces convergence to one constraint. If you ship five co-equal "root causes," a tidy single-thread chain, or a confident verdict built on unmeasured nodes — you did it wrong.

## ⚠️ This is not a copy-paste prompt skill

Unlike the other skills in this repo, why-tree's **engine is multi-agent** — it requires **Claude Code's `Workflow` tool** (parallel fan-out + independent refutation) and broad tool access (it reads the files/DBs you point it at, and the web). `PROMPT.md` is a **degraded single-context** version for runtimes without a multi-agent harness: it keeps the discipline (grade every node, refute, converge to one, honest census) but loses the parallel branch-space and the *independent* adversarial refutation that are the whole point. For the real thing, run it in Claude Code.

## Files

- `SKILL.md` — the skill (phased workflow). Place the **whole folder** in `~/.claude/skills/why-tree/` — it needs `references/` and `assets/`, not just `SKILL.md`.
- `references/methodology.md` — Goldratt CRT, the WHY-branch logic, CLR audit, constraint location, Evaporating Cloud.
- `references/answer-kinds.md` — the evidence taxonomy + grading rubric + citation rules (the core of the rigor).
- `references/workflow-template.md` — the copy-paste `Workflow` blueprint + the strict tree-JSON schema.
- `assets/tree-template.html` — the reusable interactive visualization ("The Diagnostician's Bench").
- `assets/sample-tree.html` — a non-confidential worked example (synthetic SaaS). **Predates the lever + idea panels — copy the TEMPLATE, not this sample.**
- `council-viz-design.md` — the 5-voice design rationale for the visualization (Goldratt / Feynman / Tufte / Victor / Minto).
- `tests/` — a zero-token plumbing harness + fixtures.
- `PROMPT.md` — the degraded single-context version for ChatGPT / Claude.ai / Cursor.

## Install (Claude Code)

```bash
# copy the whole folder — references/ and assets/ are required
cp -R skills/why-tree ~/.claude/skills/why-tree
```

Then invoke `/why-tree` (or just describe a hard "why are we falling short of X" problem). It will **gate on a depth choice and state the token cost before launching** — don't skip the gate.

## Use (ChatGPT / Claude.ai / Cursor)

Open `PROMPT.md`, copy the block between `===PROMPT START===` and `===PROMPT END===`, paste your problem + any evidence you can give it. You get a single-context CRT with the grading/refute/converge discipline — but not the parallel multi-agent rigor.

## Token honesty (read before launching)

Multi-agent and token-heavy by design. **Standard ≈ 22-40 agents (~0.8-2.0M tokens); Deep ≈ 90-115 agents observed (~5-7M — the 32-60 cap arithmetic is a floor that loop-until-dry + 3-vote refutes overshoot in practice); Test-plan ≈ 10-18** (real field runs — Standard: 26 agents ≈ 833k; Deep: 91 ≈ 6.6M, 111 ≈ 6.4M). The skill states this and asks for a depth before it launches. **Don't know the depth? The scout gate (~4-5 agents, ~100-150k) measures it**: 3 blind cheap passes either converge on an evidenced answer (shipped directly, swarm skipped) or demonstrate the confusability that justifies the spend — you buy the swarm on receipts, not on faith. **When NOT to use it:** a clear-cut problem a single careful pass would nail — just ask one agent. The machinery only earns its cost when **one mind could confidently pick the wrong cause** (cohort/measurement traps, several plausible roots, a mechanism hidden under a symptom, more branches than one context holds). **Test-plan mode** is for when the decisive sources are human-only (internal BI, an ERP, ad consoles): it stops at a branch-map + ranked cheapest-tests + Data Request, and you iterate via the update loop as your test results come back.

## Design constraints (do not modify)

- **Depth + token-cost gate.** Never launch the expensive tier without stating the cost and getting a depth choice. The gate is the skill's conscience.
- **Branch-space first, depth second.** Map *all* the candidate "why"s from many independent lenses before deepening any one.
- **Grade every node.** Answer-kind + confidence + citation on every node. Ungraded assertions are banned. A HYPOTHESIS must name the test that would settle it.
- **A number is not a fact.** Every MEASURED node carries a validation state (`raw`/`validated`/`triangulated`/`contested`): `raw` caps at Mod and can never support the constraint; two sources disagreeing in sign is a CONTESTED node with both values shown, never a "fact". The measurement audit attacks the pipeline (bots in the denominator, definition mismatches, seasonal Δs) — the refute pass alone won't, because a strong-looking number doesn't look thin.
- **The headline may not outrun the census.** `verdictStatus:'map'` renders an amber working-hypothesis banner in the header itself, naming the deciding test.
- **Refute before trust.** Load-bearing branches face an independent adversarial pass; killed branches stay visible *with the evidence that killed them*. (A degraded/skipped refute pass is the most dangerous failure — the engine warns loudly if it runs on zero branches.)
- **Converge to ONE.** 3-5 roots, one located system constraint — often a **policy/ownership** problem wearing a tooling costume.
- **The apex is a STATEMENT, not a question**, and never a smuggled solution ("why don't we have feature Y" biases the whole tree) — but an idea brought *for evaluation* routes to Idea Audit, it isn't refused.
- **The idea is quarantined.** In Idea Audit mode the pitch never enters a scout/lens/deepen/refute prompt or the frozen evidence brief; only the post-converge fit-check sees it — and the idea-kill is graded like any claim (WTP is a hypothesis with a test, not a pronouncement).
- **Honest census.** If the decisive nodes are still HYPOTHESIS, the headline is "a map of where to look, not a verdict," and the cheapest test targets exactly those nodes.
- **The tree must be a multi-LEVEL tree** — the constraint branch drills to bedrock as a visible chain, not a one-line root.

## Anti-patterns this skill prevents

- A single-thread 5-Whys chain with a tidy bottom (no branching, no refutation = not a tree).
- "Five key root causes" with no convergence to one constraint.
- A confident verdict resting on HYPOTHESIS-graded nodes, with no census disclosing it.
- Deleting refuted branches instead of keeping them with their killing evidence.
- Running the full multi-agent workflow on a clear-cut problem a single agent would nail.
- Smuggling the answer into the apex (a problem framed as a missing solution).
- A MEASURED chip on an unaudited pipeline — bots in the denominator, a metric-definition mismatch, or a seasonal Δ shipped as a "fact" (a field run burned 4 of 6 iterations debunking exactly these).
- An act-ready verdict header on a tree whose constraint rests on an untested node.
- Killing (or blessing) a product idea at the symptom level — the Idea Audit judges the idea against the located constraint, and generalizes an n=1 tree only via an explicit segment fork.
