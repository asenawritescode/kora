# Commissioning Brief — Canonical Template

This is the template Phase B of the skill emits. The skill fills in the `<<placeholder>>` blocks with the user's confirmed model from Phase A, then emits the whole thing as a single copy-paste block.

---

## Copy from here ↓

```
You are commissioning your AI coding agent (Claude Code or Codex) to build an interactive stock-flow simulation web app of MY model. Below is the brief. Build exactly what's specified — do not free-style architecture, do not add features I didn't ask for.

# ROLE

You are a frontend/JS engineer. I am the modeler. I own the equations; you own the infrastructure (HTML, React rendering, slider wiring, chart rendering). When in doubt about an equation, STOP and ASK. When in doubt about a layout choice, MAKE IT and we'll iterate.

# MY MODEL

Title: <<short title, e.g., "Support ticket backlog with hire-step">>

Time unit: <<weeks | months | quarters | years>>
Time horizon: <<integer, e.g., 52>>
Integration: Euler, dt = 1

## Stocks (initial values)

- <<StockName1>>: <<initial number>>  // units: <<units>>
- <<StockName2>>: <<initial number>>  // units: <<units>>

## Flows (rate equations)

- <<FlowName1>>: from <<StockName | null>> to <<StockName | null>>
  rate = <<equation in JS expression form, referring to stock and parameter names directly>>
  units: <<units, must be quantity-per-time>>

- <<FlowName2>>: from <<StockName | null>> to <<StockName | null>>
  rate = <<equation>>
  units: <<units>>

## Parameters (sliders)

- <<ParamName1>>: default <<number>>, min <<number>>, max <<number>>, step <<number>>
  label: "<<short human label, ≤20 chars>>"
  units: <<units>>

- <<ParamName2>>: default <<number>>, min <<number>>, max <<number>>, step <<number>>
  label: "<<short human label>>"
  units: <<units>>

## Reference mode

<<one or two lines describing how the primary stock has actually behaved historically and where I want it to go. The chart should be reasonable to compare against this.>>

## Pedagogy (for the Read-the-model panel — required if the spec has one)

If the skill emitted a `pedagogy` block, populate the Read-the-model panel with these contents. If absent (the user opted out), hide the panel entirely.

Insight: <<one-sentence canonical lesson — what the *model* is trying to teach>>

Loops:
- <<id, e.g., B1 or R1>> (<<balancing | reinforcing>>): <<one-line description of which flow + which mediator drives this loop>>
- <<additional loops if present>>

Try this (open-ended challenges):
- Q: <<question that requires sliding / intervening to answer>>
  A: <<short explanation, ideally with the math (eq = X × Y, etc.)>>
- <<2-4 challenges total>>

ConcepTests (Mazur peer-instruction style — multi-choice with commit-test-reveal flow):
- Q: <<question with 4 distinct options where the most-popular wrong answer is a known intuition trap>>
  Options:
    A) <<plausible wrong answer 1>>
    B) <<plausible wrong answer 2 (often "common-sense linear extrapolation")>>
    C) <<correct answer>>
    D) <<plausible wrong answer 3 (often "overshoot fear")>>
  Correct: <<index 0..3>>
  Test config: <<sliders: {ParamName: value}, interventions: [{t, paramName, newValue}]>>
  Explanation: <<what the math shows + which intuition trap the wrong answers spring; cite the time constant or equilibrium formula>>
- <<1-3 ConcepTests total>>

# DELIVERABLE

A SINGLE HTML file named `model.html`. No build step, no npm install. React via CDN (esm.sh). Recharts via CDN. The file MUST run by simply opening it in a browser (file://) — no dev server required.

## Layout

```
+-----------------------------------------------+
|  <<title>>                                    |
+-----------------------------------------------+
|  [PHASE BANNER: 'Predict' or 'Tinker'         |
|   with Reveal / Reset buttons]                |
+-----------------------------------------------+
|  [SVG diagram: stocks as boxes,               |
|   flows as arrows with labels,                |
|   information links as dashed arrows ↓        |
|   from each stock referenced in a flow's eq]  |
+-----------------------------------------------+
|  Δ at t=0 · Equilibrium · At t=horizon        |
|  · Prediction error (tinker only)             |
+-----------------------------------------------+
|  [LineChart: clickable in PREDICT phase to    |
|   add gold prediction dots; in TINKER phase   |
|   shows trajectory + dashed eq line +         |
|   intervention markers + ghost trace +        |
|   prediction dots for comparison]             |
+-----------------------------------------------+
|  [INTERVENTIONS panel — tinker only:          |
|   list of scheduled mid-run param changes     |
|   + form to add 'at t=N set <param> to <val>']|
+-----------------------------------------------+
|  Slider: <<ParamName1>> [——●——] 50            |
|         leverage-chip · Meadows #11           |
|  Slider: <<ParamName2>> [——●——] 4             |
|         leverage-chip · Meadows #9            |
|  ...                                          |
+-----------------------------------------------+
|  Equation chips (one per flow):               |
|  "<<FlowName1>>: <<equation as text>>"        |
+-----------------------------------------------+
|  [READ-THE-MODEL panel:                       |
|   - "What this model teaches" insight         |
|   - Loops list (B/R badges + descriptions)    |
|   - "Try this" challenges (collapsible)]      |
+-----------------------------------------------+
```

## Iron constraints

1. **Equations are EXACTLY what's in MY MODEL above.** Do not add `+1`, do not introduce smoothing, do not add randomness. If an equation looks wrong to you, STOP and ASK me — do not "fix" it.
2. **Every stock is clamped at zero on each step.** If a flow would push a stock negative, clamp to 0 and `console.warn` the flow name and time step. <<IF MAX-CLAMP REQUIRED: Stock <<X>> additionally clamps at <<max value>>; same warn behavior.>>
3. **Slider min/max/default come from MY MODEL above.** Do not invent ranges.
4. **Use Euler integration with dt = 1.** Each step: `new_stock = max(0, old_stock + (sum_inflows - sum_outflows) * dt)`. Order of operations: compute all flows from current state first, THEN update all stocks. (Do not update stocks in mid-step — that gives wrong dynamics.)
5. **Time-conditional convention.** When equations contain time-conditional expressions like `(t < HireWeek ? A : B)`, the threshold fires INCLUSIVELY at `t = HireWeek`. So at `t = HireWeek - 1` the condition is true (uses A); at `t = HireWeek` the condition is false (uses B). State this explicitly in a comment near the equation.
6. **Chart redraws live as sliders move.** Use React state, recompute the full simulation on every slider change (it's cheap for <100 time steps).
7. **No external state, no localStorage, no URL params.** This is a single-screen tool. Sharing comes later.
8. **Equation chips show the literal equation string from MY MODEL.** They are read-only; this is the human-readable proof that the code matches the spec. Render in modeler-friendly notation if possible — e.g., `Resolutions = min(Backlog, agents(t) * Throughput)` with a side-note `agents(t) = StartAgents if t < HireWeek else EndAgents`. Avoid raw JS ternaries in chip text if a clearer form exists.
9. **Companion test files allowed.** You may produce one `test_sim.{mjs,js}` file (or similar) that imports/inlines the simulation logic and asserts the extraction-test value. Do NOT produce alternate `model.html` variants (no Vite version, no Vue version).

## Pedagogical UI elements (MANDATORY — Sterman / Forrester / Meadows / Victor / Papert / Mazur / Case)

These ride alongside the chart and sliders to teach systems-thinking concepts, not just simulate. Implement all of them — they are the difference between a flow simulator and a system-dynamics microworld.

### Static teaching layer

1. **Information links in the diagram (Forrester).** When a flow's rate equation references a stock, draw a dashed line with arrowhead from that stock to the flow. Use a muted neutral color (e.g., `#94A3B8`). Without info links, balancing/reinforcing loops are invisible. This is the difference between drawing a flow diagram and drawing a system-dynamics diagram.

2. **Leverage-point chip on each slider (Meadows).** A small pill beside each parameter slider showing the parameter's Meadows leverage classification. Heuristic from the equation context:
   - If the parameter appears in a flow expression that ALSO references a stock (e.g., `Stock / Param`, `Stock * Param`) → "delay / rate constant · Meadows #9"
   - Otherwise (constant flow rate, parameter alone or with constants only) → "flow rate · Meadows #11"
   - If the parameter name semantically encodes a target (`*Goal`, `*Target`, `*Quota`, `*Capacity` used as a goal) → "goal · Meadows #3"
   This converts mechanism-learning into leverage-learning.

3. **Equilibrium reference line on the chart (Sterman).** Simulate to ~5× horizon (or 100 steps, whichever is greater). If the trajectory converges (last-step relative change < 0.001 and value finite), draw a dashed horizontal line at the asymptotic value, labeled "eq <value>". Use a distinct muted color (e.g., `#8B5CF6` purple). If trajectory does not converge, hide the line and mark equilibrium as "unbounded / non-convergent" in the readout. Adjust y-axis so the equilibrium line is visible.

4. **Δ at t=0 readout (Sterman).** Above the chart, show "Δ at t=0: ±N <stock-units>/<time-unit>" — the net flow at the initial state. The change is the lesson, not the level. This is the *tilt of the bath* in Sterman's pedagogy.

5. **Terminal stock readout (Sterman).** Show stock value at t=horizon alongside the equilibrium readout. Lets users compare "where did we end up over the simulated window" vs "where this is heading."

### Interactive learning loop (v0.5.0)

These four turn the simulator from a parametric viewer into a microworld with a predict→reveal→intervene→explain cycle. Implement them all.

6. **Predict-before-reveal (Sterman + Mazur).** App opens in a `predict` phase: chart axes are drawn but the trajectory is hidden, sliders are disabled (locked at defaults), only an `initial: <value>` anchor dot is shown at t=0. The user clicks 1-6 points on the chart to mark "where I think the stock will be at this t". Each click adds a gold dot at the clicked (t, value), snapped to integer t. A prominent "Reveal trajectory" button transitions to the `tinker` phase. **Animate the reveal**: trajectory draws progressively from t=0 to t=horizon over ~1.1 seconds with cubic ease-out; a glowing leader dot rides the front; equilibrium line and readouts appear at the start of the animation; sliders unlock and intervention panel appears. The animation gives the predict-vs-actual gap *time to land emotionally* — it's the difference between "you got 9% wrong" landing as a footnote versus as a moment. Predictions stay as gold dots forever (until manually cleared). Compute prediction error as average absolute |predicted − actual| against the no-intervention trajectory at current slider values, plus average percentage.

7. **Mid-run intervention scheduler (Papert + Sterman).** Below the chart in `tinker` phase, a panel where the user schedules `at t=N, set <param> to <new value>`. Multiple interventions stack. Visualize each as a vertical dashed orange line at t=N on the chart. Modify the simulation kernel: at each integration step, before computing flows, check if any intervention's t equals the current step; if so, swap that param's value in the active scope. Render a **ghost trace** (orange dashed, alpha ≈ 0.45) of the simulation *without* interventions over the same horizon, so users see "what would have been". Equilibrium calculation must apply all interventions, then continue past horizon to convergence — so eq readout reflects post-intervention destination. This is the difference between a curve viewer and a policy-experiment platform.

8. **Read-the-model panel (Case + Meadows).** Below the equation chips, a panel populated from a `pedagogy` block in the spec (skill-emitted; you must surface it as the user reads the brief and ask whether to fill it in). Four sections:
   - **What this model teaches** — one-sentence canonical insight
   - **Loops in this model** — list of {id, type: "balancing"|"reinforcing", description}; render with a colored badge per type (orange B / cyan R)
   - **ConcepTests (Mazur peer-instruction)** — list of {q, options: [{label, text}], correct: int, test: {sliders, interventions}, explanation}. Render each as a card: question + clickable option buttons; clicking an option locks the user's commitment (highlighted amber); a "Test it" button applies the test config (sets sliders + interventions) and scrolls the chart into view; a "Show answer" button marks correct option green, wrong choice red, reveals the explanation. The whole point is *commit before testing* — Mazur's peer-instruction sequence imported into a solo microworld.
   - **Try this** — list of {q, a} challenges; render each as a `<details>` block (or equivalent collapsible) so users can attempt before peeking at answers
   If the spec has no `pedagogy` block, hide the entire panel. Otherwise, this is what gives the model a narrative voice — without it, the artifact is mute about its own purpose.

9. **Hover-to-illuminate (Bret Victor).** When the user hovers a slider row, the SVG flow group(s) that reference that parameter glow (filter brightness + drop shadow), the corresponding info-link path glows, and the matching equation chip(s) get a left border highlight. Inverse: hovering a flow's SVG group or an equation chip lights up the related sliders. Implement via `data-flows="FlowA FlowB"` attributes on every linkable element + `mouseenter`/`mouseleave` handlers that toggle a `.hl` class on all elements with intersecting flow names. Cheap to implement; transforms the diagram from decoration into a queryable model surface.

These nine elements are the pedagogical contract. Implement all of them; do not ask whether to skip any.

## Stack

- React 18 from `https://esm.sh/react@18`
- ReactDOM 18 from `https://esm.sh/react-dom@18/client`
- Recharts from `https://esm.sh/recharts@2`
- No bundler. No TypeScript. Plain `.html` with one `<script type="module">` block.

# EXTRACTION TEST (you must pass before declaring done)

In a comment block at the top of the script, INCLUDE the hand-computed expected value of the primary stock at t=1 using default parameters. After implementing, run the simulation in a headless browser or by opening the file and verify the chart's value at t=1 matches the comment to within 0.01.

If the values do not match, STOP. Do not declare done. Show me the diff and we'll find where the equations diverged.

Example comment block:
```
// EXTRACTION TEST
// Defaults: Backlog=200, ArrivalRate=50, Throughput=30, StartAgents=1
// At t=0: Backlog=200
// Flows at t=0: Arrivals=50, Resolutions=min(200, 1*30)=30
// At t=1: Backlog = max(0, 200 + 50 - 30) = 220
// EXPECTED Backlog[1] = 220
```

# OUTPUT

When you're done, give me ONE thing:
1. The path to `model.html` (and open it in my browser if you can)

Do NOT also give me a Vite version, a Vue version, a Streamlit version, or a Python version. One file. One framework choice. Done.

# IF SOMETHING IS UNCLEAR

Ask me ONE question at a time. Do not paraphrase the spec back at me — I wrote it. Ask only about things genuinely missing or contradictory in MY MODEL above.
```

## ↑ Stop copying here

---

## Notes for the skill (not part of the brief)

- The skill fills in `<<placeholders>>` with the confirmed model from Phase A.
- For one-stock models, the secondary-stock dashed line is omitted from layout.
- For models without ranges on a parameter, the skill must ask the user for ranges in Phase A — the brief cannot ship with `min: TBD`.
- Equation chips intentionally show the *literal string* from MY MODEL, NOT the JS-compiled version. This is the Bret Victor authorship loop: the human can verify the agent transcribed correctly by reading one chip.
- The extraction test comment block forces the agent to do the math BEFORE wiring up the UI — catches dynamics-update-order bugs early.
