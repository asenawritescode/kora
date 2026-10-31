# `/ai-stockflow-builder` — turn your stock-flow diagram into a runnable simulation

A Claude Code skill (with a copy-paste version for Codex / ChatGPT / Cursor) that helps you commission an interactive stock-flow simulation from your AI coding agent. **The agent builds infrastructure. You own the equations.**

Built for Workshop 3 of the AI + Systems Thinking course (Empatika, 2026). Sibling to [`/ai-systems-coach`](https://github.com/BayramAnnakov/systems-thinking-skills) which grades hand-drawn diagrams (Workshop 2).

## What it does

You bring a stock-flow diagram (hand-drawn or graded by `/ai-systems-coach`). The skill walks you through three phases:

1. **Readback** — skill reads your model back in plain English, surfaces ambiguity, forces ≥1 disagreement before any code
2. **Brief** — skill emits a copy-paste commissioning brief that you paste into Claude Code or Codex; your agent builds a single-HTML-file simulation with sliders, chart, and equation chips
3. **Extraction test** — you hand-compute t=1 from defaults and compare to the agent's app; mismatch = AI broke equations = stop and find where

If you don't have a diagram, the skill also offers **Phase 0 — Interview mode**: it asks Socratic questions, your answers become the model. The skill never proposes a stock or flow you didn't name.

### Two-pass pedagogy (Pass 1 / Pass 2)

The skill enforces **Pass 1 — Simplified mode** by default for first builds:

| Pass 1 cap | Limit |
|---|---|
| Stocks | exactly 1 |
| Flows | exactly 2 (one in, one out) |
| Parameters | exactly 2 |
| Time horizon | ≤30 steps |
| Equations | linear only — no `min()`, no ternaries, no delays |

When you violate a cap, the skill runs a **trim conversation**: it proposes which entities to drop, demote to constants, or defer to Pass 2. Pass 1 forces "minimum viable model" discipline — you get something running end-to-end *fast*, then extend.

**Pass 2 — Full mode** removes the caps. Use it after Pass 1 ships, when you're extending your model with the second stock, the conditional flow, the delay you suppressed.

Why two passes: founders and managers tend to model their full-complexity intuition all at once. Pass 1 is the discipline of finding the simplest model that captures the dynamic — *then* layering complexity. The skill enforces that order.

## What you ship at the end

A single `model.html` file you can open in any browser:
- SVG stock-flow diagram of YOUR model
- Live-redraw line chart of your primary stock
- Slider for each parameter (with units)
- Equation chips showing the literal equations from your readback

No `npm install`, no dev server, no deploy. Open the file. Drag the sliders. Watch the curve.

## Install

### Option 1 — Claude Code skill (recommended)

```bash
mkdir -p ~/.claude/skills/ai-stockflow-builder
cp -r workshop3/skill/* ~/.claude/skills/ai-stockflow-builder/
# or via the systems-thinking-skills plugin marketplace once published:
# /plugin install ai-stockflow-builder@systems-thinking-skills
```

Then in any Claude Code session: `/ai-stockflow-builder`

### Option 2 — Copy-paste mega-prompt (for Codex, ChatGPT, Cursor, claude.ai)

Open `PROMPT.md`, copy everything between `===PROMPT START===` and `===PROMPT END===`, paste into your AI tool. Then paste your model and follow the three phases.

## Usage

```
/ai-stockflow-builder
```

Then paste your model in this format (or invoke interview mode):

```
Stocks:
- Backlog: 200 tickets

Flows:
- Arrivals: from null to Backlog, rate = ArrivalRate, units: tickets/week
- Resolutions: from Backlog to null, rate = min(Backlog, Agents * Throughput), units: tickets/week

Parameters:
- ArrivalRate: default 50, min 20, max 100, step 5, units: tickets/week
- Throughput: default 30, min 10, max 60, step 5, units: tickets/week/person
- Agents: default 1, min 1, max 5, step 1

Reference mode:
Backlog stayed flat at ~50 for months, surged to 200 when an agent quit.
```

Or interview mode:

```
I don't have a diagram. My problem: my SaaS churn is too high. Interview me.
```

## What the skill refuses

- Designing a diagram from scratch without your participation (interview mode requires YOU to answer questions; the skill won't pick stocks for you)
- Inventing variables you didn't name
- Skipping the readback gate ("just build it") outside express mode
- Stylistic requests during the workshop window ("make it pretty", "add animations")

## Files

| File | Purpose |
|------|---------|
| `SKILL.md` | Claude Code skill manifest + behavior spec |
| `PROMPT.md` | Copy-paste mega-prompt for non-Claude-Code AI tools |
| `briefs/canonical.md` | The commissioning brief template that Phase B emits |
| `test-cases.md` | Four worked examples for skill self-test (Stanislav backlog, Marfa focus, Vlad pipeline, Andrey interview) |
| `README.md` | This file |

## Course context

Part of the **AI + Systems Thinking** course (6 biweekly online sessions, 2026). This skill ships with **Workshop 3 — From Diagram to Simulation**, where participants take the diagram they hand-drew in Workshop 2 and turn it into runnable code.

Workshop 1 → 2 → 3 progression:
- **W1** — How systems work (stocks, flows, feedback by hand)
- **W2** — System archetypes; ships [`/ai-systems-coach`](https://github.com/BayramAnnakov/systems-thinking-skills) for grading
- **W3** — From diagram to simulation; ships **this skill**
- **W4-W6** — Theory of constraints; FishBanks; transformation

## Pedagogical philosophy

The two iron rules carry over from `/ai-systems-coach`:
1. **AI critiques and amplifies; AI does not design.**
2. **Drawing/modeling IS the act of thinking. Skipping it parasitizes understanding.**

This skill extends those rules to code commissioning: AI builds the React app, the deployment, the slider wiring. The HUMAN owns every equation. The extraction test is the human's verification that the AI didn't quietly invent dynamics.

## License

MIT — same as the parent repo.
