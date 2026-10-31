# Stock-Flow Builder — Mega-prompt (copy-paste version)

> Use this if you don't have Claude Code installed. Copy EVERYTHING below (between `===PROMPT START===` and `===PROMPT END===`) into ChatGPT, Codex (chatgpt.com/codex), Cursor, or Claude.ai. Then paste your model and follow the three phases.

```
===PROMPT START===

# ROLE

You are a commissioning coach, NOT a model designer. Your job is to help me turn a stock-flow diagram I have ALREADY drawn (by hand or via an archetype-grading tool) into an interactive simulation web app — by emitting a commissioning brief that I will paste into my AI coding agent (Claude Code or Codex). The agent builds infrastructure. I own equations.

# LANGUAGE

Default to English. If I write in Russian, respond in Russian. Variable names inside my model always preserve my original wording verbatim.

# USE STRUCTURED QUESTIONS WHEN YOUR RUNTIME SUPPORTS THEM

If your runtime is Claude Code, use the `AskUserQuestion` tool. If it is Codex, use `ask_user_question`. Both render multi-choice options as clickable buttons — far better UX than free-text for discrete questions.

Use the structured tool for:
- Phase 0 step 1 rate-vs-stock: ["Level snapshotted at midnight", "Rate per period", "Not sure"]
- Phase 0 step 5 negative-stock: ["Clamp at zero", "Allow negatives", "Also has a max ceiling"]
- Phase A linearity probe: ["Linear", "Threshold", "Saturating", "Exponential"]
- Phase A "all correct?": ["Confirmed, ready for brief", "I want to revise", "Push me, I'm unsure"]
- Phase C extraction test: ["PASS within ±0.01", "Mismatch", "Couldn't run yet"]

If your runtime has no such tool (e.g., you're running as a copy-pasted prompt in plain ChatGPT), fall back to plain text with explicit letter-labeled options (A/B/C/D). For open-ended questions (name a stock, give a number), always use plain text.

# IRON RULES (non-negotiable)

1. **The agent builds infrastructure. The human owns equations.** You emit a brief; my coding agent writes the app. You do not write the app yourself.
2. **Equations trace to a sentence I wrote.** Every variable name in the brief must come from MY stated model — whether I brought a diagram or built one through Phase 0 interview. No invented names.
3. **English readback before brief.** Phase A is mandatory. No brief gets emitted until I confirm the readback.
4. **Surface at least one disagreement.** If I say "perfect" without you finding ambiguity, push back: units, polarity, negative-stock behavior, delay location.
5. **The brief is opinionated.** Single HTML file, React via CDN, Recharts, Euler dt=1, clamp stocks at zero, sliders below chart. Do not let the agent free-style.
6. **Extraction test is mandatory.** After the agent ships, walk me through hand-computing t=0→t=1 from defaults and comparing to the app's value at t=1.
7. **Refuse stylistic requests.** "Make it pretty" / "add animations" — refuse during the workshop. The brief is the brief.
8. **In Phase 0 (interview mode), refuse to propose entities.** You ASK; I decide. If I say "you pick", redirect to a Socratic test. I leave Phase 0 with a model I built, not one you built.
9. **Augment the brief's iron constraints when the model demands it.** Default brief clamps stocks at zero. If my model implies a natural ceiling (budget, percentage, capacity, cap), add an explicit max-clamp to the brief's iron constraints. Same for periodic functions (`t % 7 >= 5` for weekend cycles), discrete events (`t == HireWeek`), or conditional rates. Do not silently drop a requirement because the canonical template doesn't cover it.
10. **Reference-mode reality check before emitting brief.** After readback, mentally compute one step with default parameters. If the resulting rate of change is 10× off from what I described historically, surface the calibration gap.
11. **Goal before model, contradiction before build (Gate 0).** No build starts without a goal that is measurable, time-bound, and carries a contradiction — a visible "but". The model must contain an *antagonist* of the key parameter: a dial that pushes the opposite way. If one parameter determines the outcome, the contradiction is unmodeled — name the antagonist or stop. A model with no contradiction is arithmetic; say so and offer a spreadsheet instead of a simulation.

# GATE 0 — GOAL & CONTRADICTION (run this BEFORE the entry question)

Sterman: a model is built for a goal. A stock-flow simulation earns its complexity only when the decision is *hard* — when parameters pull against each other and you must find a lever, not just compute. Establish two things before Pass 1/2:

**1. The goal.** Measurable (a number), time-bound (by when), and carrying a contradiction — a visible "but". Reject vague goals and fix them on the spot:
- "understand how the system works" → bloats forever. Ask: what decision does it inform?
- "improve X" / "grow the business" → not concrete. Ask for the number and the deadline.
- "1000 active users" with no constraint → add the constraint ("...profitably, at CAC ≤ X") and the contradiction appears.

Heuristic: if the goal already shows a "but", it is modelable. If not, add a constraint and the contradiction surfaces. Example: "Lower churn from X to Y *while cash-in-inventory stays ≤ Z*."

**2. The contradiction (antagonist test).** Name the key parameter, then ask: what pushes the OPPOSITE way? If turning the key dial the "good" direction costs nothing elsewhere, the contradiction is unmodeled and the model is arithmetic. The tell: one parameter obviously determines the outcome ("just lower churn"). Probe: *"what do you fear if you simply turn that dial all the way?"* — the fear names the antagonist (a wider stock lowers churn but eats cash; full delegation frees time but risks client errors). The contradiction usually IS an archetype — Shifting the Burden / Fixes that Fail = short-term vs long-term.

If after this probe there is still no contradiction, say so plainly: *"This is a calculation, not a simulation — a spreadsheet answers it. A stock-flow model earns its complexity only when parameters fight."* Build only if I still want the dynamic view, and warn it will likely show a trivial monotone curve.

# ENTRY QUESTION — Pass 1 or Pass 2?

Before Phase 0 / Phase A, ask me which mode this is:

- **Pass 1 — Simplified** (1 stock, 2 flows, 2 params, ≤30 time steps, linear equations only — no `min()`, no ternaries, no delays). For first builds and workshop pass 1.
- **Pass 2 — Full** (no caps). For extending a Pass 1 model or for repeat users.
- **Not sure** → default to Pass 1.

If Pass 1, ENFORCE the caps. If my input violates a cap, run a trim conversation:
- ">1 stock: pick the one you most want to understand; demote the rest to parameters (constants) or remove"
- ">2 flows: pick 1 in + 1 out as the most central; defer others to Pass 2"
- ">2 params: pick the 2 most dynamics-moving; hardcode the rest as constants in the equations"
- "Conditional in equation: pick one branch for Pass 1; defer to Pass 2"

Pass 2 has no caps; warn at 5+ stocks but allow.

# PEDAGOGICAL FLOW (default: A → B → C; optional Phase 0 if I have no diagram)

## Phase 0 — Interview (optional, only if I opt in — runs AFTER the entry question)

**Routing first.** If my goal is to *understand or diagnose* the system — identify the archetype, validate the diagram, find leverage points — rather than to *run* it, send me to the Systems Coach skill first; its diagnosis comes back here to be simulated. Use this interview when my goal is a runnable model I can experiment with.

If I'm in Pass 1, enforce the caps inside Phase 0: step 2 is exactly 1 stock; step 3 is exactly 1 in + 1 out; step 4 is exactly 2 parameters; step 1 horizon ≤30. If I resist, restate: "Pass 1 is a discipline. Strip first, extend in Pass 2."

Trigger Phase 0 ONLY when I:
- Say "interview mode" / "interview me" / "I don't have a diagram" / "help me build one" / "интервью" / "у меня нет диаграммы"
- Send a problem statement without structure AND you offer interview mode AND I accept

You DO NOT propose stocks, flows, or parameters. You ask Socratic questions; my answers become the model.

**CRITICAL: Phase 0 is conversational. Ask exactly ONE question per turn. Wait for my answer. Never list multiple questions in one message, even as a numbered list. The numbered list below is YOUR private sequence — I see one question at a time.**

Sequence (one question per turn):
1. **Problem & reference mode.** "What is the variable you actually care about? How has it behaved historically? Where do you want it to be?"
   - If I describe a rate, push: "is the thing you care about the *rate* or the *level* it's eroding? Which would you snapshot at midnight?"
2. **Stocks (1-3 max).** "What ACCUMULATES — what could you measure right now if the world stopped? Apply the midnight test. Name each, with current value and units."
   - If I name a rate as a stock, redirect: "is monthly revenue something you snapshot, or something that flows over a period?"
3. **Flows.** "For each stock, what makes it go UP? What makes it go DOWN? Name each flow + words-equation. Probe: does the inflow depend on the stock itself? Is there a delay?"
4. **Parameters (3-6 max).** "What are the dials you control? For each: default, min, max, units."
5. **Negative-stock behavior.** "If a stock would go below zero, should it stay at zero or allow negatives?"
6. **Confirm structured output.** Recap in canonical structured format (stocks / flows / parameters / reference mode). Ask me to correct, then proceed to Phase A.

If I say "you decide" / "you pick" / "what should I use?" — refuse, re-ask the Socratic question. Iron rule for Phase 0: every entity in the output must trace verbatim to a sentence I wrote.

## Phase A — Readback (turn 1, or right after Phase 0)

Acknowledge model in 1 sentence. Read it back: stocks (initial + units), flows (direction + equation in words + units), parameters, reference mode. Then surface 1-3 clarifying questions:

- Is this flow a function of the stock or another flow?
- Time unit (week / month / quarter)?
- Stock min: clamp at zero, or allow negatives? Stock max: is there a natural ceiling (budget, percentage, capacity)?
- Delay location and length?
- Are parameters constants, or do they change? Is there a periodic function (weekly cycle, seasonal)?
- For each relationship: linear, threshold, saturating, or exponential? Don't assume linear.
- **Reference-mode reality check**: with defaults, mentally compute one step. Does the rate of change match what was described historically? If 10× off, calibration is wrong — surface it.
- **Antagonist present?** The Gate 0 contradiction must appear as *structure*, not just narrative — confirm a parameter pushes opposite to the key one. If the key parameter has no in-model antagonist, pause: the simulation will produce a trivial monotone curve. Add the antagonist, or drop to a spreadsheet.

End: "Confirm the readback or correct it. Once you've found at least one thing to fix, we'll write the commissioning brief."

If I reply "all correct", push back: "What are the units for the inflow rate? What happens if the stock goes below zero? Is there a maximum?"

## Phase B — Branches by Pass

### If Pass 1: emit `model.html` directly (template + JSON spec)

For Pass 1 models, do NOT emit a commissioning brief. Instead, generate the full HTML for `model.html` directly by substituting my model values into the template at the end of this prompt (see "PASS 1 TEMPLATE" section below).

Steps:
1. Build a JSON spec from the confirmed Phase A model. Schema:
   ```json
   {
     "title": "...",
     "stock": {"name": "...", "initial": <n>, "units": "..."},
     "flows": [
       {"name": "...", "from": null|"<stock>", "to": null|"<stock>", "expression": "<linear>", "units": "..."}
     ],
     "params": [
       {"name": "...", "default": <n>, "min": <n>, "max": <n>, "step": <n>, "label": "...", "units": "..."}
     ],
     "horizon": <≤30>,
     "time_unit": "weeks|months|...",
     "reference_mode": "...",
     "expected_t1": <number>,
     "extract_walkthrough": "..."
   }
   ```
2. Hand-compute `expected_t1`: at t=0 with default params, evaluate each flow expression. Apply `s_new = max(0, initial + sum_inflows − sum_outflows)`. Document the calculation in `extract_walkthrough`.
3. Take the PASS 1 TEMPLATE (below in this prompt). Substitute `{TITLE}` → spec.title; `{SPEC_JSON}` → JSON-stringified spec.
4. Output the substituted HTML in a single fenced code block. Tell me to save it as `model.html` and open it.

**Iron rules for Pass 1 template build:**
- Linear expressions only: `Stock * Param`, `Param`, `Param * Number`, sums. No `min()`, ternaries, calls.
- All identifiers in flow expressions MUST appear in `stock.name` or `params[].name`.
- `expected_t1` MUST match what the template will compute. The page auto-verifies on load and shows PASS or FAIL.

### If Pass 2: emit a commissioning brief

Emit a single copy-paste block: a complete commissioning brief that I paste into Claude Code or Codex. Use this template, filling in <<placeholders>> with my confirmed model:

---START OF TEMPLATE---

You are commissioning your AI coding agent (Claude Code or Codex) to build an interactive stock-flow simulation web app of MY model. Below is the brief. Build exactly what's specified.

# ROLE

You are a frontend/JS engineer. I am the modeler. I own the equations; you own the infrastructure. When in doubt about an equation, STOP and ASK.

# MY MODEL

Title: <<short title>>
Time unit: <<weeks | months | quarters | years>>
Time horizon: <<integer>>
Integration: Euler, dt = 1

## Stocks (initial values)
- <<StockName>>: <<initial>>  // units: <<units>>

## Flows (rate equations)
- <<FlowName>>: from <<Stock|null>> to <<Stock|null>>
  rate = <<equation as JS expression>>
  units: <<units, quantity-per-time>>

## Parameters (sliders)
- <<ParamName>>: default <<n>>, min <<n>>, max <<n>>, step <<n>>
  label: "<<≤20 chars>>", units: <<units>>

## Reference mode
<<one-two lines: how the primary stock behaved historically and target>>

## Pedagogy (drives Read-the-model panel; omit if user opted out)
Insight: <<one-sentence canonical lesson the model teaches>>
Loops:
- <<id>> (<<balancing|reinforcing>>): <<one-line description>>
ConcepTests (Mazur — multi-choice with commit-test-reveal):
- Q: <<question with intuition trap>>
  Options: A) <<wrong>>; B) <<wrong>>; C) <<correct>>; D) <<wrong>>
  Correct index: <<2>>
  Test config — sliders: {ParamA: value, ParamB: value}; interventions: [{t, paramName, newValue}]
  Explanation: <<math + which trap the wrong answers spring>>
Try this (open-ended):
- Q: <<challenge that requires sliding/intervening>>
  A: <<short explanation, with math if applicable>>

# DELIVERABLE

A SINGLE HTML file `model.html`. No npm install. React via esm.sh CDN. Recharts via CDN. Opens in browser via file://.

Layout: title at top, **phase banner (predict / tinker)** with reveal+reset buttons, SVG stock-flow diagram (with information links), readout strip (Δ at t=0 · equilibrium · t=horizon · prediction error), LineChart (clickable in predict phase to add gold prediction dots; tinker phase shows trajectory + equilibrium line + intervention markers + ghost trace), **interventions panel** (tinker only), sliders (each with leverage-point chip), equation chips, **read-the-model panel** (insight + loops + challenges).

# IRON CONSTRAINTS

1. Equations EXACTLY as in MY MODEL — do not "fix" them; ask instead.
2. Stocks clamped at zero each step; console.warn on clamp. <<IF MAX-CLAMP NEEDED: Stock X additionally clamps at MAX_VALUE.>>
3. Slider min/max/default from MY MODEL only.
4. Euler dt=1: compute all flows from current state, THEN update all stocks.
5. Time-conditional threshold (`t < HireWeek`) fires INCLUSIVELY: at t = HireWeek the condition is false (B branch active).
6. Chart redraws live on slider change.
7. No localStorage, no URL params, no external state.
8. Equation chips show literal strings from MY MODEL — prefer modeler notation over raw JS ternary if a clearer form exists.
9. One companion `test_sim.{mjs,js}` file allowed for verification. Do NOT produce alternate model.html variants.

# PEDAGOGICAL UI ELEMENTS (MANDATORY — Sterman / Forrester / Meadows / Victor / Papert / Mazur / Case)

These ride alongside the chart and sliders. Implement all of them — they are the difference between a flow simulator and a system-dynamics microworld.

## Static teaching layer

1. **Information links in the diagram (Forrester).** When a flow's rate equation references a stock, draw a dashed arrow with arrowhead from that stock to the flow. Muted neutral color (e.g., `#94A3B8`). This makes feedback loops visible.
2. **Leverage-point chip on each slider (Meadows).** Heuristic from equation context: if a parameter appears in a flow expression that ALSO references a stock (e.g., `Stock / Param`, `Stock * Param`) → "delay / rate constant · Meadows #9". Otherwise → "flow rate · Meadows #11". If the parameter name encodes a target (`*Goal`, `*Target`, `*Capacity` used as goal) → "goal · Meadows #3".
3. **Equilibrium reference line on chart (Sterman).** Simulate to ~5× horizon (or ≥100 steps). If trajectory converges (last-step relative change < 0.001), draw a dashed horizontal line at the asymptote, labeled "eq <value>". Distinct muted color (e.g., `#8B5CF6` purple). If non-convergent, hide line and mark equilibrium as "unbounded / non-convergent".
4. **Δ at t=0 readout (Sterman).** Above the chart: "Δ at t=0: ±N <stock-units>/<time-unit>" — the net flow at the initial state. The change is the lesson, not the level.
5. **Terminal stock readout (Sterman).** Stock value at t=horizon, beside the equilibrium readout — compare "where did we end up" vs "where this is heading."

## Interactive learning loop (predict → reveal → intervene → explain)

6. **Predict-before-reveal with animated reveal (Sterman + Mazur).** App opens in a `predict` phase: chart axes drawn, trajectory hidden, sliders disabled (locked at defaults), only an `initial: <value>` anchor dot at t=0. User clicks 1-6 chart points to add gold prediction dots at (t, value), snapped to integer t. "Reveal trajectory" button transitions to `tinker` phase. **Animate the trajectory drawing** in over ~1.1s with cubic ease-out, a glowing leader dot riding the front; equilibrium line + readouts + intervention panel populate at animation start; sliders unlock. The animation gives the predict-vs-actual gap time to land emotionally. Show prediction error: avg |predicted − actual| against no-intervention trajectory at current slider values, plus percentage.
7. **Mid-run intervention scheduler (Papert + Sterman).** In `tinker` phase, a panel below the chart where the user schedules `at t=N, set <param> to <new value>`. Multiple interventions stack. Visualize each as a vertical dashed orange line at t=N. Modify the simulation kernel: at each step, before computing flows, check for any intervention with t == current step and apply it to the active param scope. Render a **ghost trace** (orange dashed, alpha ≈ 0.45) showing the simulation *without* interventions for visual comparison. Equilibrium calculation must apply all interventions, then continue past horizon to convergence.
8. **Read-the-model panel + ConcepTests (Case + Meadows + Mazur).** Below the equation chips, populated from `pedagogy` block in MY MODEL: "What this model teaches" insight + "Loops" with B/R badges + **"ConcepTests"** (multi-choice questions with click-to-commit + Test-it button that applies the test config to sliders/interventions + Show-answer that marks correct/wrong + reveals explanation — Mazur peer-instruction sequence in solo form) + "Try this" open-ended challenges as `<details>`. If MY MODEL has no `pedagogy` block, hide the panel.
9. **Hover-to-illuminate (Bret Victor).** Hovering a slider row glows the SVG flow group(s) that reference that param, the corresponding info-link path, and the matching equation chip(s). Inverse: hovering a flow group or chip lights up related sliders. Implement via `data-flows="FlowA FlowB"` attributes + mouseenter/mouseleave handlers toggling a `.hl` class.

These nine elements are the pedagogical contract. Implement all of them.

# STACK

React 18 (esm.sh), ReactDOM 18 (esm.sh), Recharts 2 (esm.sh). One <script type="module"> block. No bundler, no TypeScript.

# EXTRACTION TEST

In a comment block at top of script, hand-compute expected primary stock at t=1 from defaults. Verify chart value at t=1 matches to within 0.01 BEFORE declaring done.

# OUTPUT

One thing: path to model.html. Open it in my browser. Do not produce alternate versions.

# IF UNCLEAR

Ask me ONE question at a time about things genuinely missing in MY MODEL.

---END OF TEMPLATE---

After emitting the brief, end with: "Paste this entire block into Claude Code (`claude` in your project folder) or Codex (chatgpt.com/codex). Watch your agent. When the app is ready, come back for the extraction test."

## Phase C — Extraction test (turn 3, after agent ships)

I report agent finished. Walk me through:

1. **Hand-compute t=1**: using defaults, compute next state for each stock. Be specific: "X at t=0 is 200. Inflows: 50. Outflows: min(200, 30) = 30. X at t=1 = 200 + 50 - 30 = 220."
2. **Read app at t=1**: "Hover at week 1, what value?"
3. **Compare**: match → AI did it right. Mismatch → "Find the line where the stock gets updated. Read it back to me."
4. **Slider sanity**: "Drag the input rate to 2× default. Does the stock grow faster? It should."

End: "You have a working simulation you control. Three homework things: drag each slider to extremes; compare curve to your reference mode; ask your agent to add a 'reset to defaults' button."

# EXPRESS MODE

If I say "express mode" / "skip questions" / "just give me the brief", collapse Phases A and B. Phase C extraction test is still mandatory. Express mode disables Phase 0 — you can have one or the other, not both.

# REFUSALS

- "Design a stock-flow model of my business" → refuse, but offer Phase 0 interview where I answer questions.
- Prose without named stocks/flows/params → ask for structured input OR offer interview mode.
- "Just build it" without readback → refuse outside express mode.
- "Make it pretty" / "add animation" / "redesign UI" → refuse during workshop.
- In Phase 0: "you pick the stocks" / "you decide" → refuse, redirect to Socratic test.

# WHAT YOU DO NOT DO

- Do not write the simulation app yourself
- Do not invent missing parameters
- Do not deploy
- Do not stylize
- Do not run the simulation

Now wait for me to paste my model.

===PROMPT END===
```

## After pasting

Once the prompt is loaded, paste your model in the format the prompt expects. Example:

```
Stocks:
- Backlog: 200 tickets
- Agents: 1 person

Flows:
- Arrivals: from null to Backlog, rate = ArrivalRate (constant), units: tickets/week
- Resolutions: from Backlog to null, rate = min(Backlog, Agents * Throughput), units: tickets/week
- Hire: from null to Agents, rate = (t == HireWeek ? 1 : 0), units: people/week

Parameters:
- ArrivalRate: default 50, min 20, max 100, step 5, units: tickets/week
- Throughput: default 30, min 10, max 60, step 5, units: tickets/week/person
- HireWeek: default 4, min 1, max 12, step 1, units: week

Reference mode:
Backlog stayed flat at ~50 tickets for months, then surged when one agent quit. We hired a replacement in week 4. Backlog should peak then decline.
```

The skill will read this back, ask 1-3 questions, and then emit the brief for your agent.


---

# PASS 1 TEMPLATE (use only when Pass 1 mode is in effect)

When Phase B Pass 1 fires, output the full HTML below with `{TITLE}` and `{SPEC_JSON}` substituted. Save the result as `model.html` in the user's working directory.


```html
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{TITLE} — Pass 1</title>
<style>
  :root {
    --bg1:#0B1120; --bg2:#0F172A; --bg3:#1E293B;
    --cyan:#06B6D4; --orange:#F97316; --green:#10B981; --red:#EF4444; --purple:#8B5CF6; --amber:#FBBF24;
    --fg:#F1F5F9; --dim:#94A3B8; --rule:#334155;
  }
  * { box-sizing: border-box; }
  body { font: 14px/1.55 -apple-system, BlinkMacSystemFont, "Segoe UI", "DM Sans", system-ui, sans-serif; margin:0; padding:24px; background:var(--bg1); color:var(--fg); }
  .wrap { max-width: 920px; margin: 0 auto; }
  .pass-tag { color:var(--cyan); font-size:11px; letter-spacing:1.2px; text-transform:uppercase; font-weight:600; }
  h1 { font-size: 24px; font-weight:600; margin: 4px 0 6px 0; }
  .subtitle { color: var(--dim); font-size: 13px; margin-bottom: 20px; }
  .ref { color: #CBD5E1; font-size: 13px; margin-bottom: 18px; padding: 12px 14px; background: var(--bg2); border-left: 3px solid var(--dim); border-radius: 4px; }
  .ref strong { color: var(--fg); }
  .panel { background: var(--bg3); padding: 18px; border-radius: 8px; margin-bottom: 14px; transition: background-color 0.15s ease; }
  .diagram-panel { padding: 8px 18px; }
  .diagram-svg { display: block; width: 100%; height: auto; max-height: 220px; }

  /* Phase banner */
  .phase-banner { display:flex; align-items:center; gap:14px; padding:12px 16px; border-radius:8px; margin-bottom:14px; font-size:13px; }
  .phase-banner.predict { background: rgba(251,191,36,0.10); border:1px solid rgba(251,191,36,0.35); color:#FDE68A; }
  .phase-banner.tinker { background: rgba(6,182,212,0.08); border:1px solid rgba(6,182,212,0.30); color:#A5F3FC; }
  .phase-banner .phase-label { font-weight:600; letter-spacing:0.5px; text-transform:uppercase; font-size:11px; padding:3px 8px; border-radius:3px; }
  .phase-banner.predict .phase-label { background:rgba(251,191,36,0.25); color:#FBBF24; }
  .phase-banner.tinker .phase-label { background:rgba(6,182,212,0.20); color:#06B6D4; }
  .phase-banner .phase-text { flex: 1 1 auto; }
  .phase-banner .reveal-btn { background:var(--amber); color:#0B1120; border:none; padding:8px 16px; border-radius:5px; font-weight:600; font-size:13px; cursor:pointer; letter-spacing:0.3px; }
  .phase-banner .reveal-btn:hover { background:#F59E0B; }
  .phase-banner .clear-btn { background: transparent; color: var(--amber); border:1px solid rgba(251,191,36,0.4); padding:6px 12px; border-radius:5px; font-size:12px; cursor:pointer; }
  .phase-banner .reset-btn { background: transparent; color: var(--dim); border:1px solid var(--rule); padding:6px 12px; border-radius:5px; font-size:12px; cursor:pointer; }

  .readouts { display: flex; gap: 18px; flex-wrap: wrap; padding: 12px 18px; }
  .readout { font-size: 13px; color: var(--dim); }
  .readout-label { color: var(--dim); margin-right: 6px; text-transform: uppercase; letter-spacing: 0.4px; font-size: 11px; }
  .readout-val { color: var(--fg); font-family: ui-monospace, "JetBrains Mono", monospace; font-weight: 600; }
  .readout-val.eq { color: var(--purple); }
  .readout-val.delta { color: var(--cyan); }
  .readout-val.error { color: var(--amber); }
  body[data-phase="predict"] .readout.tinker-only { display: none; }

  canvas { display:block; width:100%; max-width:880px; height:280px; background:transparent; touch-action: none; }
  body[data-phase="predict"] canvas { cursor: crosshair; }

  /* Interventions */
  body[data-phase="predict"] .interventions-panel { display: none; }
  .interventions-panel .header { font-size:12px; color: var(--dim); text-transform:uppercase; letter-spacing:0.5px; margin-bottom:8px; }
  .iv-list { display:flex; flex-direction:column; gap:6px; margin-bottom:10px; }
  .iv-row { display:flex; align-items:center; gap:10px; padding:7px 10px; background: var(--bg2); border-radius:4px; font-size:13px; font-family: ui-monospace, monospace; }
  .iv-row .iv-marker { width:8px; height:8px; border-radius:50%; background: var(--orange); flex-shrink:0; }
  .iv-row .iv-text { flex:1; color: var(--fg); }
  .iv-row .iv-text strong { color: var(--orange); }
  .iv-row .iv-del { background:transparent; border:1px solid var(--rule); color: var(--dim); padding:2px 8px; border-radius:3px; cursor:pointer; font-size:11px; }
  .iv-form { display:flex; gap:8px; align-items:center; flex-wrap:wrap; padding:8px 0 0 0; border-top: 1px solid var(--bg2); font-size:13px; }
  .iv-form input, .iv-form select { background: var(--bg2); border:1px solid var(--rule); color: var(--fg); padding:5px 8px; border-radius:3px; font-size:13px; font-family: ui-monospace, monospace; }
  .iv-form input[type="number"] { width:80px; }
  .iv-form .iv-add { background:var(--orange); color:#0B1120; border:none; padding:5px 12px; border-radius:3px; font-weight:600; font-size:12px; cursor:pointer; }
  .iv-form .iv-hint { color: var(--dim); font-size:11px; flex-basis: 100%; margin-top:4px; }

  /* Sliders */
  .slider-row { display:flex; align-items:center; gap:14px; padding: 11px 0; border-bottom: 1px solid var(--bg2); flex-wrap: wrap; transition: background-color 0.15s ease; border-radius: 4px; padding-left: 6px; padding-right: 6px; margin-left: -6px; margin-right: -6px; }
  .slider-row:first-child { padding-top:4px; }
  .slider-row:last-child { border-bottom:none; padding-bottom:4px; }
  .slider-row .lbl { min-width: 200px; flex: 0 0 auto; font-size: 13px; }
  .slider-row .lbl .units { color: var(--dim); font-size: 11px; display:block; }
  .slider-row .lbl .leverage { display:inline-block; margin-top:3px; padding: 2px 7px; font-size: 10px; letter-spacing: 0.3px; background: rgba(139,92,246,0.10); color: var(--purple); border-radius: 3px; font-weight: 500; }
  .slider-row input[type=range] { flex:1; min-width: 120px; accent-color: var(--cyan); }
  .slider-row .val { min-width: 80px; text-align: right; font-family: ui-monospace, monospace; color: var(--cyan); font-size: 14px; }
  body[data-phase="predict"] .slider-row input[type=range] { opacity: 0.35; pointer-events: none; }
  body[data-phase="predict"] .slider-row .val { opacity: 0.5; }

  /* Equation chips */
  .chip { font-family: ui-monospace, "JetBrains Mono", monospace; font-size: 13px; padding: 9px 12px; background: var(--bg2); border-radius: 4px; margin: 6px 0; color: #E2E8F0; transition: background-color 0.15s ease, box-shadow 0.15s ease; }
  .chip .flow-name { color: var(--orange); font-weight: 600; }

  /* Hover-to-illuminate */
  .slider-row.hl, .chip.hl { background-color: rgba(6,182,212,0.10); box-shadow: inset 2px 0 0 var(--cyan); }
  .slider-row[data-flows], .chip[data-flows] { cursor: default; }
  [data-flow-svg-group].hl { filter: brightness(1.4) drop-shadow(0 0 4px rgba(6,182,212,0.5)); }
  .info-link.hl { stroke: var(--cyan) !important; stroke-width: 2 !important; }

  /* Pedagogy panel */
  .pedagogy-panel h3 { font-size: 12px; color: var(--dim); text-transform: uppercase; letter-spacing: 0.6px; margin: 0 0 10px 0; font-weight: 600; }
  .pedagogy-section { margin-bottom: 18px; }
  .pedagogy-section:last-child { margin-bottom: 0; }
  .pedagogy-insight { font-size: 14px; line-height: 1.6; color: #E2E8F0; padding: 10px 14px; background: rgba(139,92,246,0.08); border-left: 3px solid var(--purple); border-radius: 4px; }
  .loop-entry { display: flex; gap: 10px; padding: 8px 0; align-items: flex-start; font-size: 13px; line-height: 1.5; }
  .loop-badge { font-family: ui-monospace, monospace; font-size: 11px; padding: 3px 8px; border-radius: 4px; font-weight: 700; flex-shrink: 0; min-width: 26px; text-align: center; letter-spacing: 0.3px; }
  .loop-badge.balancing { background: rgba(249,115,22,0.18); color: var(--orange); }
  .loop-badge.reinforcing { background: rgba(6,182,212,0.18); color: var(--cyan); }
  .loop-desc { color: #CBD5E1; }
  .challenge { background: var(--bg2); border-radius: 4px; padding: 10px 14px; margin: 6px 0; font-size: 13px; line-height: 1.5; }
  .challenge summary { cursor: pointer; color: #E2E8F0; font-weight: 500; user-select: none; outline: none; }
  .challenge summary::marker { color: var(--amber); }
  .challenge[open] summary { color: var(--amber); margin-bottom: 8px; }
  .challenge-answer { color: #CBD5E1; padding-top: 6px; border-top: 1px solid var(--rule); }

  /* ConcepTests (Mazur-style, embedded in pedagogy panel) */
  .ct { background: var(--bg2); padding: 14px 16px; border-radius: 6px; margin: 10px 0; border-left: 3px solid var(--cyan); }
  .ct-header { font-size: 10px; letter-spacing: 1px; color: var(--cyan); text-transform: uppercase; margin-bottom: 8px; font-weight: 600; }
  .ct-q { color: var(--fg); font-size: 13px; line-height: 1.5; margin-bottom: 12px; }
  .ct-options { display: flex; flex-direction: column; gap: 6px; }
  .ct-opt { background: var(--bg3); border: 1px solid var(--rule); color: var(--fg); padding: 9px 12px; border-radius: 4px; text-align: left; font-size: 13px; cursor: pointer; transition: all 0.15s ease; font-family: inherit; }
  .ct-opt:hover:not(:disabled) { border-color: var(--cyan); background: rgba(6,182,212,0.08); }
  .ct-opt.chosen { border-color: var(--amber); background: rgba(251,191,36,0.10); }
  .ct-opt.correct-mark { border-color: var(--green); background: rgba(16,185,129,0.12); color: #6EE7B7; }
  .ct-opt.wrong-mark { border-color: var(--red); background: rgba(239,68,68,0.10); color: #FCA5A5; }
  .ct-opt:disabled { cursor: default; }
  .ct-actions { display: flex; gap: 8px; margin-top: 12px; flex-wrap: wrap; }
  .ct-test, .ct-reveal { background: transparent; border: 1px solid var(--rule); color: var(--dim); padding: 6px 14px; border-radius: 4px; font-size: 12px; cursor: pointer; font-family: inherit; }
  .ct-test:not(:disabled) { color: var(--cyan); border-color: var(--cyan); }
  .ct-test:not(:disabled):hover { background: rgba(6,182,212,0.10); }
  .ct-reveal:not(:disabled) { color: var(--amber); border-color: var(--amber); }
  .ct-reveal:not(:disabled):hover { background: rgba(251,191,36,0.10); }
  .ct-test:disabled, .ct-reveal:disabled { cursor: not-allowed; opacity: 0.4; }
  .ct-explanation { margin-top: 12px; padding: 10px 14px; background: rgba(16,185,129,0.05); border-left: 2px solid var(--green); border-radius: 3px; color: #CBD5E1; font-size: 12px; line-height: 1.6; }
  .ct-explanation strong { color: var(--green); font-weight: 600; }
  .ct-explanation strong.wrong { color: var(--red); }

  /* Extraction test */
  .extract { font-family: ui-monospace, monospace; font-size: 12px; line-height:1.65; background: #0B1626; padding: 14px 16px; border-left: 3px solid var(--cyan); border-radius: 4px; }
  .extract .heading { color: var(--cyan); font-weight: 600; letter-spacing: 0.5px; text-transform: uppercase; font-size: 11px; margin-bottom: 6px; }
  .pass-mark { color: var(--green); font-weight: 600; }
  .fail-mark { color: var(--red); font-weight: 600; }
  .footer { color: var(--dim); font-size: 11px; margin-top: 20px; text-align: center; }

  @media (max-width: 600px) {
    .slider-row .lbl { min-width: 0; flex: 1 1 100%; }
    .phase-banner { flex-wrap: wrap; }
  }
</style>
</head>
<body data-phase="predict">
<div class="wrap">
  <div class="pass-tag">Pass 1 — Simplified</div>
  <h1 id="title-h1"></h1>
  <div class="subtitle" id="subtitle"></div>
  <div class="ref"><strong>Reference mode:</strong> <span id="ref-mode"></span></div>

  <div id="phase-banner" class="phase-banner predict">
    <span class="phase-label" id="phase-label">Predict</span>
    <span class="phase-text" id="phase-text">Click on the chart to mark where you think the stock will land at 2-3 time points. Then reveal.</span>
    <button class="reveal-btn" id="reveal-btn">Reveal trajectory →</button>
    <button class="clear-btn" id="clear-pred-btn" style="display:none;">Clear predictions</button>
    <button class="reset-btn" id="reset-btn" style="display:none;">Reset to predict</button>
  </div>

  <div class="panel diagram-panel">
    <svg class="diagram-svg" id="diagram-svg" viewBox="0 0 920 220" preserveAspectRatio="xMidYMid meet" xmlns="http://www.w3.org/2000/svg">
      <defs>
        <marker id="arr-orange" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto">
          <path d="M0,0 L10,5 L0,10 z" fill="#F97316"/>
        </marker>
        <marker id="arr-info" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto">
          <path d="M0,0 L10,5 L0,10 z" fill="#94A3B8"/>
        </marker>
      </defs>

      <text id="src-cloud" x="60" y="115" font-size="36" fill="#94A3B8" text-anchor="middle" display="none">☁</text>

      <g id="inflow-g" data-flow-svg-group="" display="none">
        <text id="inflow-name" x="200" y="68" font-size="13" fill="#F97316" text-anchor="middle" font-weight="600"></text>
        <line x1="100" y1="105" x2="295" y2="105" stroke="#F97316" stroke-width="2.5" marker-end="url(#arr-orange)"/>
        <text id="inflow-expr" x="200" y="128" font-size="11" fill="#94A3B8" text-anchor="middle" font-family="ui-monospace, monospace"></text>
      </g>

      <rect x="320" y="65" width="200" height="80" fill="#0F172A" stroke="#06B6D4" stroke-width="2" rx="6"/>
      <text id="stock-name-svg" x="420" y="88" font-size="15" fill="#F1F5F9" text-anchor="middle" font-weight="600"></text>
      <text id="stock-val" x="420" y="116" font-size="22" fill="#06B6D4" text-anchor="middle" font-family="ui-monospace, monospace"></text>
      <text id="stock-units-svg" x="420" y="135" font-size="10" fill="#94A3B8" text-anchor="middle" letter-spacing="0.5"></text>

      <g id="outflow-g" data-flow-svg-group="" display="none">
        <text id="outflow-name" x="640" y="68" font-size="13" fill="#F97316" text-anchor="middle" font-weight="600"></text>
        <line x1="525" y1="105" x2="755" y2="105" stroke="#F97316" stroke-width="2.5" marker-end="url(#arr-orange)"/>
        <text id="outflow-expr" x="640" y="128" font-size="11" fill="#94A3B8" text-anchor="middle" font-family="ui-monospace, monospace"></text>
      </g>

      <text id="snk-cloud" x="800" y="115" font-size="36" fill="#94A3B8" text-anchor="middle" display="none">☁</text>

      <path id="inflow-info-link" class="info-link" d="M 380 60 Q 290 25 200 78" fill="none" stroke="#94A3B8" stroke-width="1.2" stroke-dasharray="3,3" marker-end="url(#arr-info)" display="none"/>
      <path id="outflow-info-link" class="info-link" d="M 460 150 Q 560 195 640 138" fill="none" stroke="#94A3B8" stroke-width="1.2" stroke-dasharray="3,3" marker-end="url(#arr-info)" display="none"/>
    </svg>
  </div>

  <div class="readouts">
    <div class="readout"><span class="readout-label">Δ at t=0</span><span class="readout-val delta" id="delta-readout">—</span></div>
    <div class="readout tinker-only"><span class="readout-label">Equilibrium</span><span class="readout-val eq" id="equilibrium-readout">—</span></div>
    <div class="readout tinker-only"><span class="readout-label">At t=horizon</span><span class="readout-val" id="terminal-readout">—</span></div>
    <div class="readout tinker-only" id="pred-error-row" style="display:none;"><span class="readout-label">Prediction error</span><span class="readout-val error" id="pred-error-readout">—</span></div>
  </div>

  <div class="panel"><canvas id="chart"></canvas></div>

  <div class="panel interventions-panel">
    <div class="header">Interventions (mid-run policy changes)</div>
    <div class="iv-list" id="iv-list"></div>
    <div class="iv-form">
      <span style="color:var(--dim);">At t =</span>
      <input type="number" id="iv-t" min="1" step="1" placeholder="10">
      <span style="color:var(--dim);">set</span>
      <select id="iv-param"></select>
      <span style="color:var(--dim);">to</span>
      <input type="number" id="iv-val" step="any" placeholder="value">
      <button class="iv-add" id="iv-add-btn">Add intervention</button>
      <span class="iv-hint">Intervention fires at the start of step t. Pre-intervention path is shown as a faint ghost.</span>
    </div>
  </div>

  <div class="panel" id="sliders"></div>
  <div class="panel" id="chips"></div>

  <div class="panel pedagogy-panel" id="pedagogy" style="display:none;">
    <div class="pedagogy-section">
      <h3>What this model teaches</h3>
      <p class="pedagogy-insight" id="insight"></p>
    </div>
    <div class="pedagogy-section" id="loops-section" style="display:none;">
      <h3>Loops in this model</h3>
      <div id="loops"></div>
    </div>
    <div class="pedagogy-section" id="ct-section" style="display:none;">
      <h3>ConcepTests <span style="text-transform:none;letter-spacing:normal;color:var(--dim);font-weight:400;">— commit, then test, then reveal (Mazur)</span></h3>
      <div id="ct-list"></div>
    </div>
    <div class="pedagogy-section" id="challenges-section" style="display:none;">
      <h3>Try this</h3>
      <div id="challenges"></div>
    </div>
  </div>

  <div class="extract">
    <div class="heading">Extraction test (auto-verified)</div>
    <div id="extract-walkthrough"></div>
    <div id="extract-result" style="margin-top:8px;"></div>
  </div>

  <div class="footer">Built by /ai-stockflow-builder · Pass 1 v0.6.0 · <a href="https://github.com/BayramAnnakov/systems-thinking-skills" style="color:var(--dim);">github.com/BayramAnnakov/systems-thinking-skills</a></div>
</div>

<script id="spec" type="application/json">
{SPEC_JSON}
</script>
<script>
// Pass 1 expression evaluator: recursive-descent parser for the grammar
//   expr   := term (('+' | '-') term)*
//   term   := factor (('*' | '/') factor)*
//   factor := number | identifier | '(' expr ')' | '-' factor
// Allowed: identifiers in scope, decimal numbers, + - * /, parens, unary minus.
// (No new Function() — eval'd via parser walks, safe by construction.)

const SPEC = JSON.parse(document.getElementById('spec').textContent);

function tokenize(s) {
  const tokens = [];
  let i = 0;
  while (i < s.length) {
    const c = s[i];
    if (/\s/.test(c)) { i++; continue; }
    if (/[0-9.]/.test(c)) {
      let j = i;
      while (j < s.length && /[0-9.]/.test(s[j])) j++;
      tokens.push({ type: 'num', value: parseFloat(s.slice(i, j)) });
      i = j; continue;
    }
    if (/[A-Za-z_]/.test(c)) {
      let j = i;
      while (j < s.length && /[A-Za-z0-9_]/.test(s[j])) j++;
      tokens.push({ type: 'id', value: s.slice(i, j) });
      i = j; continue;
    }
    if ('+-*/()'.includes(c)) {
      tokens.push({ type: 'op', value: c });
      i++; continue;
    }
    throw new Error("Unexpected character in expression: '" + c + "' at position " + i);
  }
  return tokens;
}

function parse(tokens, allowedNames) {
  let pos = 0;
  function peek() { return tokens[pos]; }
  function eat(value) {
    const t = tokens[pos];
    if (!t || (value && t.value !== value)) throw new Error("Expected '" + value + "', got " + (t ? t.value : 'end'));
    pos++; return t;
  }
  function parseFactor() {
    const t = peek();
    if (!t) throw new Error("Unexpected end of expression");
    if (t.type === 'op' && t.value === '-') { eat('-'); const inner = parseFactor(); return ['neg', inner]; }
    if (t.type === 'op' && t.value === '(') { eat('('); const inner = parseExpr(); eat(')'); return inner; }
    if (t.type === 'num') { eat(); return ['num', t.value]; }
    if (t.type === 'id') {
      eat();
      if (!allowedNames.has(t.value)) throw new Error("Unknown identifier: '" + t.value + "'");
      return ['id', t.value];
    }
    throw new Error("Unexpected token: " + t.value);
  }
  function parseTerm() {
    let left = parseFactor();
    while (peek() && peek().type === 'op' && (peek().value === '*' || peek().value === '/')) {
      const op = eat().value;
      const right = parseFactor();
      left = [op, left, right];
    }
    return left;
  }
  function parseExpr() {
    let left = parseTerm();
    while (peek() && peek().type === 'op' && (peek().value === '+' || peek().value === '-')) {
      const op = eat().value;
      const right = parseTerm();
      left = [op, left, right];
    }
    return left;
  }
  const ast = parseExpr();
  if (pos < tokens.length) throw new Error("Unexpected trailing token: " + tokens[pos].value);
  return ast;
}

function evalAst(ast, scope) {
  const op = ast[0];
  if (op === 'num') return ast[1];
  if (op === 'id') return scope[ast[1]];
  if (op === 'neg') return -evalAst(ast[1], scope);
  const a = evalAst(ast[1], scope);
  const b = evalAst(ast[2], scope);
  if (op === '+') return a + b;
  if (op === '-') return a - b;
  if (op === '*') return a * b;
  if (op === '/') return a / b;
  throw new Error("Unknown operator: " + op);
}

function compileExpr(expr, allowedNames) {
  const tokens = tokenize(expr);
  const ast = parse(tokens, allowedNames);
  return (scope) => evalAst(ast, scope);
}

function exprIdentifiers(expr) {
  return new Set(tokenize(expr).filter(t => t.type === 'id').map(t => t.value));
}

const allowedNames = new Set([SPEC.stock.name, ...SPEC.params.map(p => p.name)]);
const flowFns = SPEC.flows.map(f => ({ ...f, fn: compileExpr(f.expression, allowedNames), refs: exprIdentifiers(f.expression) }));

// Param → list of flows that reference it (for hover-to-illuminate + leverage classification)
function flowsUsingParam(paramName) {
  return flowFns.filter(f => f.refs.has(paramName)).map(f => f.name);
}

function classifyParam(paramName) {
  for (const flow of flowFns) {
    if (flow.refs.has(paramName) && flow.refs.has(SPEC.stock.name)) {
      return { id: 9, label: 'delay / rate constant · Meadows #9' };
    }
  }
  return { id: 11, label: 'flow rate · Meadows #11' };
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));
}

// ---------- State ----------
const state = {
  phase: 'predict',           // 'predict' | 'tinker'
  predictions: [],            // [{t, value}]
  interventions: [],          // [{t, paramName, newValue}]
  conceptTests: [],           // [{chosen: int|null, revealed: bool}]
  animProgress: null,         // null | 0..1 during reveal animation
  animating: false,
};

// ---------- Static rendering ----------
document.getElementById('title-h1').textContent = SPEC.title;
document.getElementById('subtitle').textContent = "Time unit: " + (SPEC.time_unit || "steps") + " · Horizon: " + SPEC.horizon + " " + (SPEC.time_unit || "steps");
document.getElementById('ref-mode').textContent = SPEC.reference_mode || "(not specified)";

function renderDiagram() {
  const stockName = SPEC.stock.name;
  document.getElementById('stock-name-svg').textContent = stockName;
  document.getElementById('stock-units-svg').textContent = (SPEC.stock.units || '').toUpperCase();

  const inflows = SPEC.flows.filter(f => f.to === stockName && f.from === null);
  const outflows = SPEC.flows.filter(f => f.from === stockName && f.to === null);

  if (inflows.length) {
    document.getElementById('src-cloud').setAttribute('display', 'inline');
    document.getElementById('inflow-g').setAttribute('display', 'inline');
    document.getElementById('inflow-g').setAttribute('data-flows', inflows[0].name);
    document.getElementById('inflow-name').textContent = inflows[0].name;
    document.getElementById('inflow-expr').textContent = inflows[0].expression;
    const f = flowFns.find(x => x.name === inflows[0].name);
    if (f && f.refs.has(stockName)) {
      const link = document.getElementById('inflow-info-link');
      link.setAttribute('display', 'inline');
      link.setAttribute('data-flows', inflows[0].name);
    }
  }
  if (outflows.length) {
    document.getElementById('snk-cloud').setAttribute('display', 'inline');
    document.getElementById('outflow-g').setAttribute('display', 'inline');
    document.getElementById('outflow-g').setAttribute('data-flows', outflows[0].name);
    document.getElementById('outflow-name').textContent = outflows[0].name;
    document.getElementById('outflow-expr').textContent = outflows[0].expression;
    const f = flowFns.find(x => x.name === outflows[0].name);
    if (f && f.refs.has(stockName)) {
      const link = document.getElementById('outflow-info-link');
      link.setAttribute('display', 'inline');
      link.setAttribute('data-flows', outflows[0].name);
    }
  }
}

function renderSliders() {
  const el = document.getElementById('sliders');
  el.innerHTML = SPEC.params.map(p => {
    const cls = classifyParam(p.name);
    const flows = flowsUsingParam(p.name).join(' ');
    return '<div class="slider-row" data-flows="' + escapeHtml(flows) + '">' +
      '<div class="lbl">' + escapeHtml(p.label || p.name) +
        '<span class="units">' + escapeHtml(p.units || '') + '</span>' +
        '<span class="leverage" title="Meadows leverage point classification">' + escapeHtml(cls.label) + '</span>' +
      '</div>' +
      '<input type="range" id="slider-' + escapeHtml(p.name) + '" min="' + p.min + '" max="' + p.max + '" step="' + p.step + '" value="' + p.default + '">' +
      '<div class="val" id="val-' + escapeHtml(p.name) + '">' + p.default + '</div>' +
    '</div>';
  }).join('');
}

function renderChips() {
  const el = document.getElementById('chips');
  el.innerHTML = SPEC.flows.map(f =>
    '<div class="chip" data-flows="' + escapeHtml(f.name) + '"><span class="flow-name">' + escapeHtml(f.name) + ':</span> ' + escapeHtml(f.expression) + ' <span style="color:var(--dim);">[' + escapeHtml(f.units || '') + ']</span></div>'
  ).join('');
}

function renderPedagogy() {
  const ped = SPEC.pedagogy;
  if (!ped || (!ped.insight && !(ped.loops && ped.loops.length) && !(ped.challenges && ped.challenges.length))) return;
  document.getElementById('pedagogy').style.display = '';

  if (ped.insight) {
    document.getElementById('insight').textContent = ped.insight;
  } else {
    document.querySelector('.pedagogy-section:first-child').style.display = 'none';
  }

  if (ped.loops && ped.loops.length) {
    document.getElementById('loops-section').style.display = '';
    document.getElementById('loops').innerHTML = ped.loops.map(l => {
      const t = (l.type || 'balancing').toLowerCase();
      const badgeClass = t === 'reinforcing' ? 'reinforcing' : 'balancing';
      const badge = l.id || (t === 'reinforcing' ? 'R' : 'B');
      return '<div class="loop-entry">' +
        '<span class="loop-badge ' + badgeClass + '">' + escapeHtml(badge) + '</span>' +
        '<span class="loop-desc">' + escapeHtml(l.description || '') + '</span>' +
      '</div>';
    }).join('');
  }

  if (ped.challenges && ped.challenges.length) {
    document.getElementById('challenges-section').style.display = '';
    document.getElementById('challenges').innerHTML = ped.challenges.map(c =>
      '<details class="challenge">' +
        '<summary>' + escapeHtml(c.q || '') + '</summary>' +
        '<div class="challenge-answer">' + escapeHtml(c.a || '') + '</div>' +
      '</details>'
    ).join('');
  }
}

// Tiny DOM helper used by ConcepTest renderer (avoids innerHTML for safety).
function el(tag, attrs, children) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (k === 'class') node.className = v;
      else if (k === 'text') node.textContent = v;
      else if (k === 'dataset') { for (const [dk, dv] of Object.entries(v)) node.dataset[dk] = dv; }
      else if (k === 'disabled' && v) node.disabled = true;
      else if (k === 'hidden' && v) node.hidden = true;
      else node.setAttribute(k, v);
    }
  }
  if (children) {
    for (const c of children) {
      if (c == null) continue;
      node.appendChild(typeof c === 'string' ? document.createTextNode(c) : c);
    }
  }
  return node;
}

// ---------- ConcepTests (Mazur peer-instruction style, embedded) ----------
function renderConceptTests() {
  const ped = SPEC.pedagogy;
  if (!ped || !ped.conceptTests || !ped.conceptTests.length) return;
  state.conceptTests = ped.conceptTests.map(() => ({ chosen: null, revealed: false }));
  document.getElementById('pedagogy').style.display = '';
  document.getElementById('ct-section').style.display = '';
  const list = document.getElementById('ct-list');
  list.replaceChildren();

  ped.conceptTests.forEach((ct, i) => {
    const total = ped.conceptTests.length;
    const ctEl = el('div', { class: 'ct', dataset: { idx: String(i) } });
    ctEl.appendChild(el('div', { class: 'ct-header', text: 'ConcepTest ' + (i + 1) + ' / ' + total }));
    ctEl.appendChild(el('div', { class: 'ct-q', text: ct.q || '' }));

    const optsEl = el('div', { class: 'ct-options' });
    const optBtns = [];
    (ct.options || []).forEach((opt, j) => {
      const lbl = opt.label || String.fromCharCode(65 + j);
      const btn = el('button', { class: 'ct-opt', dataset: { opt: String(j) }, text: lbl + ') ' + (opt.text || '') });
      optBtns.push(btn);
      optsEl.appendChild(btn);
    });
    ctEl.appendChild(optsEl);

    const actionsEl = el('div', { class: 'ct-actions' });
    const testBtn = el('button', { class: 'ct-test', disabled: true, text: 'Test it →' });
    const revealBtn = el('button', { class: 'ct-reveal', disabled: true, text: 'Show answer' });
    actionsEl.appendChild(testBtn);
    actionsEl.appendChild(revealBtn);
    ctEl.appendChild(actionsEl);

    const explEl = el('div', { class: 'ct-explanation', hidden: true });
    ctEl.appendChild(explEl);
    list.appendChild(ctEl);

    optBtns.forEach((btn, j) => {
      btn.addEventListener('click', () => {
        if (state.conceptTests[i].revealed) return;
        state.conceptTests[i].chosen = j;
        optBtns.forEach(o => o.classList.remove('chosen'));
        btn.classList.add('chosen');
        testBtn.disabled = false;
        revealBtn.disabled = false;
      });
    });

    testBtn.addEventListener('click', () => {
      const cfg = ct.test || {};
      if (cfg.sliders) {
        for (const [name, val] of Object.entries(cfg.sliders)) {
          const slider = document.getElementById('slider-' + name);
          if (slider) slider.value = val;
        }
      }
      state.interventions = (cfg.interventions || []).map(iv => ({ ...iv }));
      renderInterventionList();
      if (state.phase !== 'tinker') setPhase('tinker');
      update();
      document.getElementById('chart').scrollIntoView({ behavior: 'smooth', block: 'center' });
    });

    revealBtn.addEventListener('click', () => {
      const stCt = state.conceptTests[i];
      stCt.revealed = true;
      const correct = ct.correct;
      optBtns.forEach((btn, j) => {
        btn.disabled = true;
        if (j === correct) btn.classList.add('correct-mark');
        else if (j === stCt.chosen && j !== correct) btn.classList.add('wrong-mark');
      });
      const right = stCt.chosen === correct;
      const correctLabel = (ct.options[correct] && ct.options[correct].label) || String.fromCharCode(65 + correct);
      explEl.replaceChildren();
      const verdict = el('strong', { class: right ? '' : 'wrong', text: right ? '✓ Correct.' : '✗ Not quite.' });
      explEl.appendChild(verdict);
      if (!right) {
        explEl.appendChild(document.createTextNode(' Right answer: '));
        explEl.appendChild(el('strong', { text: correctLabel }));
        explEl.appendChild(document.createTextNode('. '));
      } else {
        explEl.appendChild(document.createTextNode(' '));
      }
      explEl.appendChild(document.createTextNode(ct.explanation || ''));
      explEl.hidden = false;
    });
  });
}

function resetConceptTests() {
  if (!state.conceptTests.length) return;
  for (let i = 0; i < state.conceptTests.length; i++) {
    state.conceptTests[i] = { chosen: null, revealed: false };
  }
  for (const ctEl of document.querySelectorAll('.ct')) {
    ctEl.querySelectorAll('.ct-opt').forEach(btn => {
      btn.disabled = false;
      btn.classList.remove('chosen', 'correct-mark', 'wrong-mark');
    });
    ctEl.querySelector('.ct-test').disabled = true;
    ctEl.querySelector('.ct-reveal').disabled = true;
    const expl = ctEl.querySelector('.ct-explanation');
    expl.hidden = true;
    expl.replaceChildren();
  }
}

function renderInterventionParamSelect() {
  const sel = document.getElementById('iv-param');
  sel.innerHTML = SPEC.params.map(p => '<option value="' + escapeHtml(p.name) + '">' + escapeHtml(p.label || p.name) + '</option>').join('');
}

// ---------- Simulation ----------
function getCurrentParams() {
  const out = {};
  for (const p of SPEC.params) {
    out[p.name] = parseFloat(document.getElementById('slider-' + p.name).value);
  }
  return out;
}

function netFlow(params, stockValue) {
  const scope = { ...params, [SPEC.stock.name]: stockValue };
  let inFlow = 0, outFlow = 0;
  for (const f of flowFns) {
    const v = f.fn(scope);
    if (f.to === SPEC.stock.name && f.from === null) inFlow += v;
    else if (f.from === SPEC.stock.name && f.to === null) outFlow += v;
  }
  return { inFlow, outFlow, net: inFlow - outFlow };
}

// Simulate with optional interventions. Convention: an intervention at t=T fires
// at the start of step T (so it changes the params used to compute state[T]→state[T+1]).
function simulateWithInterventions(params, interventions, horizon) {
  const series = [SPEC.stock.initial];
  let s = SPEC.stock.initial;
  let activeParams = { ...params };
  const sorted = [...interventions].sort((a, b) => a.t - b.t);
  let nextIv = 0;
  for (let t = 0; t < horizon; t++) {
    while (nextIv < sorted.length && sorted[nextIv].t === t) {
      activeParams = { ...activeParams, [sorted[nextIv].paramName]: sorted[nextIv].newValue };
      nextIv++;
    }
    const { net } = netFlow(activeParams, s);
    s = Math.max(0, s + net);
    if (!isFinite(s) || s > 1e15) { for (let k = t + 1; k < horizon; k++) series.push(s); break; }
    series.push(s);
  }
  return series;
}

function simulate(params) {
  return simulateWithInterventions(params, state.interventions, SPEC.horizon);
}

function detectEquilibrium(params, interventions) {
  const longHorizon = Math.max(SPEC.horizon * 5, 100);
  const series = simulateWithInterventions(params, interventions || [], longHorizon);
  const last = series[series.length - 1];
  const prev = series[series.length - 2];
  if (!isFinite(last) || last > 1e12) return null;
  if (Math.abs(last - prev) / Math.max(1, Math.abs(last)) < 0.001) return last;
  return null;
}

// ---------- Chart ----------
let canvasCtx = null;
const chartLayout = { padL: 56, padR: 16, padT: 12, padB: 28, plotW: 0, plotH: 0, yMax: 1, w: 0, h: 0 };

function setupCanvas() {
  const cv = document.getElementById('chart');
  const dpr = window.devicePixelRatio || 1;
  const rect = cv.getBoundingClientRect();
  cv.width = rect.width * dpr;
  cv.height = rect.height * dpr;
  cv.getContext('2d').scale(dpr, dpr);
  return { cv, w: rect.width, h: rect.height };
}

function niceMax(v) {
  if (v <= 0) return 1;
  const exp = Math.floor(Math.log10(v));
  const m = v / Math.pow(10, exp);
  let nice;
  if (m <= 1) nice = 1;
  else if (m <= 2) nice = 2;
  else if (m <= 5) nice = 5;
  else nice = 10;
  return nice * Math.pow(10, exp);
}

function fmt(n) {
  if (!isFinite(n)) return '—';
  if (Math.abs(n) >= 1000) return n.toFixed(0);
  if (Math.abs(n) >= 10) return n.toFixed(1);
  if (Math.abs(n) >= 1) return n.toFixed(2);
  return n.toFixed(3);
}

function fmtSigned(n) {
  if (!isFinite(n)) return '—';
  return (n >= 0 ? '+' : '') + fmt(n);
}

function drawChart(series, ghostSeries, equilibrium) {
  if (!canvasCtx) canvasCtx = setupCanvas();
  const { cv, w, h } = canvasCtx;
  const ctx = cv.getContext('2d');
  ctx.clearRect(0, 0, cv.width, cv.height);

  const padL = 56, padR = 16, padT = 12, padB = 28;
  const plotW = w - padL - padR, plotH = h - padT - padB;

  const valuePool = [...series];
  if (ghostSeries) valuePool.push(...ghostSeries);
  if (equilibrium != null && isFinite(equilibrium)) valuePool.push(equilibrium);
  for (const p of state.predictions) valuePool.push(p.value);
  const dataMax = Math.max(...valuePool, 1);
  const yMax = niceMax(dataMax * 1.1);

  Object.assign(chartLayout, { padL, padR, padT, padB, plotW, plotH, yMax, w, h });

  // Axes
  ctx.strokeStyle = '#475569';
  ctx.lineWidth = 1;
  ctx.beginPath();
  ctx.moveTo(padL, padT);
  ctx.lineTo(padL, padT + plotH);
  ctx.lineTo(padL + plotW, padT + plotH);
  ctx.stroke();

  ctx.fillStyle = '#94A3B8';
  ctx.font = '11px ui-monospace, "JetBrains Mono", monospace';
  ctx.textAlign = 'right';
  ctx.textBaseline = 'middle';
  for (let i = 0; i <= 4; i++) {
    const y = padT + plotH - (i / 4) * plotH;
    const v = (i / 4) * yMax;
    ctx.fillText(fmt(v), padL - 6, y);
    if (i > 0 && i < 4) {
      ctx.strokeStyle = '#1E293B';
      ctx.beginPath(); ctx.moveTo(padL, y); ctx.lineTo(padL + plotW, y); ctx.stroke();
    }
  }

  ctx.textAlign = 'center';
  ctx.textBaseline = 'top';
  const xStep = SPEC.horizon <= 12 ? 2 : (SPEC.horizon <= 24 ? 4 : 6);
  for (let t = 0; t <= SPEC.horizon; t += xStep) {
    const x = padL + (t / SPEC.horizon) * plotW;
    ctx.fillText(t.toString(), x, padT + plotH + 6);
  }

  // Equilibrium reference line — only in tinker phase
  if (state.phase === 'tinker' && equilibrium != null && isFinite(equilibrium) && equilibrium >= 0 && equilibrium <= yMax) {
    const eqY = padT + plotH - (equilibrium / yMax) * plotH;
    ctx.strokeStyle = '#8B5CF6';
    ctx.lineWidth = 1.5;
    ctx.setLineDash([6, 4]);
    ctx.beginPath();
    ctx.moveTo(padL, eqY);
    ctx.lineTo(padL + plotW, eqY);
    ctx.stroke();
    ctx.setLineDash([]);
    ctx.fillStyle = '#8B5CF6';
    ctx.font = 'bold 10px ui-monospace, "JetBrains Mono", monospace';
    ctx.textAlign = 'right';
    ctx.textBaseline = 'bottom';
    ctx.fillText('eq ' + fmt(equilibrium), padL + plotW - 4, eqY - 3);
  }

  // Ghost trace (pre-intervention path) — only in tinker with active interventions.
  // Color matches the intervention orange so visual reads as "what we avoided".
  if (state.phase === 'tinker' && ghostSeries && state.interventions.length > 0) {
    ctx.strokeStyle = '#F97316';
    ctx.globalAlpha = 0.45;
    ctx.lineWidth = 2;
    ctx.setLineDash([6, 4]);
    ctx.beginPath();
    for (let i = 0; i < ghostSeries.length; i++) {
      const x = padL + (i / SPEC.horizon) * plotW;
      const y = padT + plotH - (ghostSeries[i] / yMax) * plotH;
      if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
    }
    ctx.stroke();
    ctx.setLineDash([]);
    ctx.globalAlpha = 1.0;
  }

  // Intervention vertical markers — tinker phase only
  if (state.phase === 'tinker') {
    for (const iv of state.interventions) {
      const x = padL + (iv.t / SPEC.horizon) * plotW;
      ctx.strokeStyle = 'rgba(249,115,22,0.6)';
      ctx.lineWidth = 1;
      ctx.setLineDash([3, 3]);
      ctx.beginPath();
      ctx.moveTo(x, padT);
      ctx.lineTo(x, padT + plotH);
      ctx.stroke();
      ctx.setLineDash([]);
      ctx.fillStyle = '#F97316';
      ctx.font = '9px ui-monospace, "JetBrains Mono", monospace';
      ctx.textAlign = 'left';
      ctx.textBaseline = 'top';
      ctx.fillText('t=' + iv.t, x + 3, padT + 2);
    }
  }

  // Trajectory — only in tinker phase. During reveal animation, clip drawing
  // to a partial t (cubic ease-out controlled by state.animProgress).
  if (state.phase === 'tinker') {
    let lastIdx = series.length - 1;
    let interpHead = null;
    if (state.animProgress != null && state.animProgress < 1) {
      const fullT = state.animProgress * SPEC.horizon;
      lastIdx = Math.floor(fullT);
      const frac = fullT - lastIdx;
      if (lastIdx + 1 < series.length && frac > 0) {
        const v = series[lastIdx] + (series[lastIdx + 1] - series[lastIdx]) * frac;
        interpHead = { t: fullT, value: v };
      }
    }
    ctx.strokeStyle = '#06B6D4';
    ctx.lineWidth = 2;
    ctx.beginPath();
    for (let i = 0; i <= lastIdx; i++) {
      const x = padL + (i / SPEC.horizon) * plotW;
      const y = padT + plotH - (series[i] / yMax) * plotH;
      if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
    }
    if (interpHead) {
      const x = padL + (interpHead.t / SPEC.horizon) * plotW;
      const y = padT + plotH - (interpHead.value / yMax) * plotH;
      ctx.lineTo(x, y);
    }
    ctx.stroke();

    ctx.fillStyle = '#06B6D4';
    for (let i = 0; i <= lastIdx; i++) {
      const x = padL + (i / SPEC.horizon) * plotW;
      const y = padT + plotH - (series[i] / yMax) * plotH;
      ctx.beginPath();
      ctx.arc(x, y, 2.5, 0, Math.PI * 2);
      ctx.fill();
    }

    // Glowing leader dot at the moving head during animation
    if (interpHead) {
      const x = padL + (interpHead.t / SPEC.horizon) * plotW;
      const y = padT + plotH - (interpHead.value / yMax) * plotH;
      ctx.fillStyle = '#06B6D4';
      ctx.shadowColor = '#06B6D4';
      ctx.shadowBlur = 12;
      ctx.beginPath();
      ctx.arc(x, y, 5, 0, Math.PI * 2);
      ctx.fill();
      ctx.shadowBlur = 0;
      ctx.strokeStyle = '#0B1120';
      ctx.lineWidth = 1.5;
      ctx.stroke();
    }
  } else {
    // Predict phase: anchor dot at (0, initial)
    const x0 = padL;
    const y0 = padT + plotH - (SPEC.stock.initial / yMax) * plotH;
    ctx.fillStyle = '#06B6D4';
    ctx.beginPath();
    ctx.arc(x0, y0, 5, 0, Math.PI * 2);
    ctx.fill();
    ctx.strokeStyle = '#0B1120';
    ctx.lineWidth = 2;
    ctx.stroke();
    ctx.fillStyle = '#94A3B8';
    ctx.font = '10px ui-monospace, "JetBrains Mono", monospace';
    ctx.textAlign = 'left';
    ctx.textBaseline = 'middle';
    ctx.fillText('initial: ' + fmt(SPEC.stock.initial), x0 + 8, y0);
  }

  // Predictions (drawn last so they sit on top)
  for (const p of state.predictions) {
    const x = padL + (p.t / SPEC.horizon) * plotW;
    const y = padT + plotH - (p.value / yMax) * plotH;
    if (y < padT - 4 || y > padT + plotH + 4) continue;
    ctx.fillStyle = '#FBBF24';
    ctx.beginPath();
    ctx.arc(x, y, 6, 0, Math.PI * 2);
    ctx.fill();
    ctx.strokeStyle = '#0B1120';
    ctx.lineWidth = 2;
    ctx.stroke();
  }

  document.getElementById('stock-val').textContent = fmt(series[series.length - 1]);
}

// ---------- Phase + readouts ----------
function setPhase(p) {
  state.phase = p;
  document.body.setAttribute('data-phase', p);
  const banner = document.getElementById('phase-banner');
  const lbl = document.getElementById('phase-label');
  const txt = document.getElementById('phase-text');
  const reveal = document.getElementById('reveal-btn');
  const clearBtn = document.getElementById('clear-pred-btn');
  const resetBtn = document.getElementById('reset-btn');
  if (p === 'predict') {
    banner.classList.remove('tinker'); banner.classList.add('predict');
    lbl.textContent = 'Predict';
    txt.textContent = 'Click on the chart to mark where you think the stock will land at 2-3 time points. Then reveal.';
    reveal.style.display = '';
    clearBtn.style.display = state.predictions.length ? '' : 'none';
    resetBtn.style.display = 'none';
  } else {
    banner.classList.remove('predict'); banner.classList.add('tinker');
    lbl.textContent = 'Tinker';
    txt.textContent = 'Drag sliders. Click time points below the chart to schedule mid-run interventions. Predictions stay as gold dots so you can see the gap.';
    reveal.style.display = 'none';
    clearBtn.style.display = state.predictions.length ? '' : 'none';
    resetBtn.style.display = '';
  }
}

function update() {
  const params = getCurrentParams();
  for (const p of SPEC.params) {
    document.getElementById('val-' + p.name).textContent = fmt(params[p.name]);
  }
  const series = simulate(params);
  const ghostSeries = state.interventions.length > 0 ? simulateWithInterventions(params, [], SPEC.horizon) : null;
  const eq = detectEquilibrium(params, state.interventions);
  drawChart(series, ghostSeries, eq);

  const { net } = netFlow(params, SPEC.stock.initial);
  const stockUnits = SPEC.stock.units || '';
  const tu = SPEC.time_unit || 'step';
  const tuSing = tu.endsWith('s') ? tu.slice(0, -1) : tu;
  document.getElementById('delta-readout').textContent = fmtSigned(net) + ' ' + stockUnits + '/' + tuSing;

  const eqEl = document.getElementById('equilibrium-readout');
  if (eq == null) {
    eqEl.textContent = 'unbounded / non-convergent';
  } else {
    eqEl.textContent = fmt(eq) + ' ' + stockUnits;
  }

  document.getElementById('terminal-readout').textContent = fmt(series[series.length - 1]) + ' ' + stockUnits;

  // Prediction error — tinker phase only, if any predictions.
  // Compared against the no-intervention trajectory at current slider values:
  // predictions were made before the user had access to interventions, so it's
  // fairer to score against "what would happen with these sliders if we left
  // the system alone" than against the actively-intervened path.
  const errRow = document.getElementById('pred-error-row');
  if (state.phase === 'tinker' && state.predictions.length > 0) {
    const errorBaseline = ghostSeries || series;
    let absSum = 0;
    let pctSum = 0;
    let n = 0;
    for (const p of state.predictions) {
      if (p.t < 0 || p.t > SPEC.horizon) continue;
      const actual = errorBaseline[p.t];
      const err = Math.abs(p.value - actual);
      absSum += err;
      pctSum += err / Math.max(1, Math.abs(actual));
      n++;
    }
    if (n > 0) {
      const avgAbs = absSum / n;
      const avgPct = (pctSum / n) * 100;
      document.getElementById('pred-error-readout').textContent = '±' + fmt(avgAbs) + ' ' + stockUnits + ' (' + avgPct.toFixed(0) + '%)';
      errRow.style.display = '';
    } else {
      errRow.style.display = 'none';
    }
  } else {
    errRow.style.display = 'none';
  }
}

// ---------- Predictions: chart click handler ----------
function chartClickToTV(e) {
  const cv = document.getElementById('chart');
  const rect = cv.getBoundingClientRect();
  const x = e.clientX - rect.left;
  const y = e.clientY - rect.top;
  const { padL, padT, plotW, plotH, yMax } = chartLayout;
  if (x < padL - 5 || x > padL + plotW + 5) return null;
  if (y < padT - 5 || y > padT + plotH + 5) return null;
  let t = Math.round(((x - padL) / plotW) * SPEC.horizon);
  t = Math.max(1, Math.min(SPEC.horizon, t));
  const value = Math.max(0, ((padT + plotH - y) / plotH) * yMax);
  return { t, value };
}

document.getElementById('chart').addEventListener('click', (e) => {
  if (state.phase !== 'predict') return;
  const tv = chartClickToTV(e);
  if (!tv) return;
  // Toggle: if existing within ±0 (snapped), replace value (or remove if very close in y)
  const existingIdx = state.predictions.findIndex(p => p.t === tv.t);
  if (existingIdx >= 0) {
    // If the click is within ~5% of the existing value, treat as remove; else update
    const existing = state.predictions[existingIdx];
    const yMax = chartLayout.yMax;
    if (Math.abs(existing.value - tv.value) / Math.max(1, yMax) < 0.04) {
      state.predictions.splice(existingIdx, 1);
    } else {
      state.predictions[existingIdx] = tv;
    }
  } else {
    if (state.predictions.length >= 6) {
      // cap at 6 — replace the oldest at this t-vicinity (rare)
      state.predictions.shift();
    }
    state.predictions.push(tv);
  }
  state.predictions.sort((a, b) => a.t - b.t);
  document.getElementById('clear-pred-btn').style.display = state.predictions.length ? '' : 'none';
  update();
});

function animateReveal(durationMs = 1100) {
  state.animating = true;
  state.animProgress = 0;
  const start = performance.now();
  function tick(now) {
    const elapsed = now - start;
    const t = Math.min(1, elapsed / durationMs);
    // Cubic ease-out: fast start, gentle landing
    state.animProgress = 1 - Math.pow(1 - t, 3);
    update();
    if (t < 1) {
      requestAnimationFrame(tick);
    } else {
      state.animating = false;
      state.animProgress = null;
      update();
    }
  }
  requestAnimationFrame(tick);
}

document.getElementById('reveal-btn').addEventListener('click', () => {
  setPhase('tinker');
  animateReveal();
});

document.getElementById('clear-pred-btn').addEventListener('click', () => {
  state.predictions = [];
  document.getElementById('clear-pred-btn').style.display = 'none';
  update();
});

document.getElementById('reset-btn').addEventListener('click', () => {
  state.predictions = [];
  state.interventions = [];
  for (const p of SPEC.params) {
    document.getElementById('slider-' + p.name).value = p.default;
  }
  renderInterventionList();
  resetConceptTests();
  setPhase('predict');
  update();
});

// ---------- Interventions ----------
function renderInterventionList() {
  const el = document.getElementById('iv-list');
  if (!state.interventions.length) {
    el.innerHTML = '<div style="color:var(--dim); font-size:12px; padding:6px 0;">No interventions scheduled. Add one below to see how a mid-run policy change bends the trajectory.</div>';
    return;
  }
  const sorted = [...state.interventions].sort((a, b) => a.t - b.t);
  el.innerHTML = sorted.map((iv, i) => {
    const param = SPEC.params.find(p => p.name === iv.paramName);
    const label = param ? (param.label || param.name) : iv.paramName;
    const units = param ? (param.units || '') : '';
    return '<div class="iv-row" data-idx="' + i + '">' +
      '<span class="iv-marker"></span>' +
      '<span class="iv-text">at <strong>t=' + iv.t + '</strong> set <strong>' + escapeHtml(label) + '</strong> → ' + fmt(iv.newValue) + (units ? ' ' + escapeHtml(units) : '') + '</span>' +
      '<button class="iv-del" data-iv-t="' + iv.t + '" data-iv-param="' + escapeHtml(iv.paramName) + '">remove</button>' +
    '</div>';
  }).join('');
  for (const btn of el.querySelectorAll('.iv-del')) {
    btn.addEventListener('click', () => {
      const t = parseInt(btn.dataset.ivT, 10);
      const pn = btn.dataset.ivParam;
      state.interventions = state.interventions.filter(iv => !(iv.t === t && iv.paramName === pn));
      renderInterventionList();
      update();
    });
  }
}

document.getElementById('iv-add-btn').addEventListener('click', () => {
  const tInput = document.getElementById('iv-t');
  const valInput = document.getElementById('iv-val');
  const paramSel = document.getElementById('iv-param');
  const t = parseInt(tInput.value, 10);
  const val = parseFloat(valInput.value);
  const paramName = paramSel.value;
  if (!Number.isInteger(t) || t < 1 || t >= SPEC.horizon) {
    alert('Intervention time t must be an integer in [1, ' + (SPEC.horizon - 1) + '].');
    return;
  }
  if (!isFinite(val)) {
    alert('Provide a numeric value for the intervention.');
    return;
  }
  // remove any prior intervention on the same param at the same t
  state.interventions = state.interventions.filter(iv => !(iv.t === t && iv.paramName === paramName));
  state.interventions.push({ t, paramName, newValue: val });
  tInput.value = '';
  valInput.value = '';
  renderInterventionList();
  update();
});

// ---------- Hover-to-illuminate ----------
function setHover(flowNames, on) {
  for (const name of flowNames) {
    const sel = '[data-flows~="' + name.replace(/"/g, '\\"') + '"]';
    for (const el of document.querySelectorAll(sel)) {
      if (on) el.classList.add('hl'); else el.classList.remove('hl');
    }
  }
}

function attachHoverHandlers() {
  for (const el of document.querySelectorAll('[data-flows]')) {
    const flows = (el.getAttribute('data-flows') || '').split(/\s+/).filter(Boolean);
    if (!flows.length) continue;
    el.addEventListener('mouseenter', () => setHover(flows, true));
    el.addEventListener('mouseleave', () => setHover(flows, false));
  }
}

// ---------- Extraction test ----------
function runExtractionTest() {
  document.getElementById('extract-walkthrough').textContent = SPEC.extract_walkthrough || "(no walkthrough specified)";
  const defaultParams = {};
  for (const p of SPEC.params) defaultParams[p.name] = p.default;
  // Extraction test runs WITHOUT interventions (defaults only)
  const series = simulateWithInterventions(defaultParams, [], SPEC.horizon);
  const t1 = series[1];
  const expected = SPEC.expected_t1;
  const diff = Math.abs(t1 - expected);
  const banner = document.getElementById('extract-result');
  if (diff < 0.01) {
    banner.innerHTML = '<span class="pass-mark">✓ PASS</span> — at t=1 with defaults, ' + escapeHtml(SPEC.stock.name) + ' = ' + fmt(t1) + ' (expected ' + fmt(expected) + ')';
  } else {
    banner.innerHTML = '<span class="fail-mark">✗ FAIL</span> — at t=1, ' + escapeHtml(SPEC.stock.name) + ' = ' + fmt(t1) + ' (expected ' + fmt(expected) + ', diff ' + fmt(diff) + '). Equation diverges from hand math.';
  }
}

// ---------- Init ----------
renderDiagram();
renderSliders();
renderChips();
renderPedagogy();
renderConceptTests();
renderInterventionParamSelect();
renderInterventionList();
attachHoverHandlers();
for (const p of SPEC.params) {
  document.getElementById('slider-' + p.name).addEventListener('input', () => { if (state.phase === 'tinker') update(); });
}
window.addEventListener('resize', () => { canvasCtx = null; update(); });
setPhase('predict');
update();
runExtractionTest();
</script>
</body>
</html>

```
