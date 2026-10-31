# Council: Why-Tree visualization — "follow & validate" vs "decide" (+ the two-expand wart)
Date: 2026-06-19
Panel: 5 (Goldratt, Feynman, Tufte, Bret Victor, Minto)

## The Panel
| Name | Lens | Known bias |
|------|------|-----------|
| Eliyahu Goldratt | ToC / CRT / focusing to ONE constraint; logic must survive CLR scrutiny | over-weights the single constraint; treats "understanding" as subordinate to action |
| Richard Feynman | don't fool yourself; understanding = ability to retrace/rebuild | over-weights first-principles tracing; under-weights decision speed |
| Edward Tufte | macro+micro in ONE display; data-ink; detail-on-demand; anti-chartjunk | over-weights static print density; distrusts interaction |
| Bret Victor | explorable explanations; ladder of abstraction; semantic zoom; show-your-work | over-engineers; over-weights novel interaction |
| Barbara Minto | Pyramid Principle: answer-first, MECE, governing-thought-then-support | over-weights top-down answer-first; under-weights bottom-up discovery |

## Verdict Matrix
| Persona | Rating (single view serves both) | Key quote |
|---|---|---|
| Goldratt | 6/10 (4 without a persistent spine minimap) | "Validation is not a second view — it is a second ZOOM LEVEL on the same node." |
| Feynman | 6/10 | "The truth lives in the edges… an AI fools you in the joints, not the facts." |
| Tufte | 8/10 | "Don't build two views. Build one watershed, and draw the water." |
| Bret Victor | 9/10 | "Decide and validate are the same ladder at different rungs. Two panes is giving up on the ladder." |
| Minto | 4/10 single VIEW; 9/10 single ARTIFACT, two MODES | "Two modes, one governing thought. That is the pyramid." |

## Key Agreements (high-confidence signal — all five)
1. **The org-chart is semantically WRONG: it draws CONTAINMENT ("A owns B"), but a CRT means CAUSATION/CONVERGENCE ("many causes → one effect").** The 5,000px sprawl is the symptom — containment has no reason to converge. Draw convergence as convergence (lines that merge), never as indentation.
2. **The two-expand wart is a SEMANTIC error, not a visual one.** Content and children are different verbs. Fix: **children = the click (descend); content (full text + evidence + the refutation that survived) = selection/zoom → a side rail, never a box-mutating click.** Kill the text-expand gesture. Prose leaves the canvas (which also cures the width).
3. **Evidence-grade belongs on the SPINE/edges, visible at decide-altitude** — the weakest link on the critical path should glow/pale so "your constraint rests on 2 hypotheses" is a pre-attentive scan, not a tooltip dig.
4. **Traceability = a LIT PATH, not a panel.** Click the constraint → highlight the single causal spine to its evidence, dim the rest. "The graph IS the provenance."
5. **Default = the spine** (constraint + critical path + surviving roots), refuted branches kept as **ghost-stubs** (the graveyard is validation-information — proof the AI tried to break itself). Not fully expanded, not fully collapsed.
6. **"Decide" and "validate" are NOT co-equal needs** — they're the same act at two zoom levels, or two PHASES (validate once, decide repeatedly). Every panelist rejected the "balance them" framing.

## Key Disagreements (where the insight lives)
- **One continuous view (Tufte 8, Victor 9) vs. two explicit modes (Minto 4→9).** Tufte/Victor: one semantic-zoom field, "no modes." Minto: a single always-on view is the wrong unit; you need ONE artifact with a "decide" mode (apex-first) and a "validate" mode (climb-up). Goldratt splits the difference: one view ONLY if a persistent spine-minimap survives every zoom; else fall back to two panes.
- **Orientation — top vs bottom.** Minto: constraint at the APEX on top (answer-first; presentation ≠ discovery order). Goldratt/Tufte/Victor: watershed — causes converge DOWN/IN to the constraint. Feynman: left-to-right convergence DAG. (Reconcilable: a top-down funnel that *narrows* to the constraint = answer pinned at top as a verdict banner, geometry converging to the constraint node = our current skeleton.)
- **Reasoning-replay: soul or theater?** Victor: the build-replay (fan-out→grade→refute→converge scrubber) is "the soul of the tool" — watching branches die is where trust is born. Goldratt: "theater, not the front door" — opt-in deep-dive for the skeptic's 2nd visit. Tufte: distrusts animation. → replay = opt-in, not default.

**Most surprising:** Feynman & Victor's **edge-first** reframe — the AI doesn't fool you with wrong *facts* (checkable), it fools you in the *joints* (a plausible cause→effect arrow that's actually a non-sequitur, graded CLAIM but load-bearing). So grade the ARROWS, make the weak load-bearing arrow glow. Plus Victor's "drag an evidence chip MEASURED→HYPOTHESIS and watch the constraint MOVE" — make fragility pokeable.

## Meta-Analyst Review (groupthink check)
- **Shared assumptions (unchallenged by all 5):** (a) the tree's logic is roughly RIGHT and the constraint correctly located — nobody asked "what if the convergence-to-ONE is itself the bug" (false funnel, or genuinely multiple constraints); (b) the founder is a SOLO, LIVE, INTERACTIVE reader — nobody considered the **sendable/async artifact** (this tree gets Slacked to a team, a client, an investor who wasn't there); (c) interaction is available at the moment of use — much real consumption is a frozen screenshot/PDF.
- **Missing perspective:** the **async/mobile reader**, the **red-teamer trying to BREAK the tree**, and a **Bezos-style "kill the chart, write the one-paragraph narrative + table"** voice.
- **Overconfident consensus:** "semantic zoom solves it" — but Feynman & Goldratt both flagged that **most users won't fluently operate semantic zoom** ("two views wearing a trenchcoat"). The consensus rests on a zoom-fluency assumption that may not hold.
- **The unasked question:** **How does the view visually distinguish a constraint that is PROVEN from one that is merely located-but-hypothesized?** A confident funnel graphic asserts certainty the census (often "constraint-locating nodes are HYPOTHESIS") doesn't support — the viz can fool the founder the same way the skill warns the AI can fool itself.

## Synthesis & Recommendation
**Ship ONE view — the convergence spine (our skeleton) — as the default "decide" surface, and make "validate" a drill-down ON the same spine, not a second layout.** Concretely:
1. **Keep the top-down funnel** (apex → narrowing → ONE constraint at the bottom; verdict pinned at top). It already draws convergence, fits a screen, answers "decide" in 5s with zero interaction.
2. **Fix the two-expand wart (the user's question):** click a node = **descend** (reveal its causes in the spine); full text + evidence + surviving-refutation = a **side rail driven by selection/hover**, never a box-mutating click. One gesture per surface. Prose lives in the rail, labels live on the canvas.
3. **Put the weakest evidence on the critical path on the SPINE** (pale/dashed link) so the founder sees the constraint's fragility at decide-altitude — this directly answers the Meta-Analyst's unasked question (proven vs hypothesized constraint).
4. **Click the constraint → light the full causal path to its evidence**, dim the rest (traceability = a lit line).
5. **Refuted branches = ghost-stubs** you can summon (the graveyard = trust).
6. **Reasoning-replay = opt-in scrubber** (build later, only if observed distrust). Goldratt's persistent **spine-minimap** is the safety rail if zoom is added.

**Does it hold if the Meta-Analyst is right (constraint not actually single/proven)?** Yes — and it improves the design: the spine must encode constraint *confidence* (solid vs dashed glow tied to the census), so the funnel never launders a hypothesis as a verdict.

## Prototype FIRST (panel's pick)
The **static converged spine + click-to-descend + selection-driven side rail + weakest-link-on-spine + click-constraint-to-light-path.** No animation, no replay yet. This single artifact serves decide (default) and validate (drill) and fixes the wart. Add the replay scrubber and the "poke the evidence, watch the constraint move" only after watching real distrust.

## What Needs Real Validation (not AI opinion)
- Do founders actually drill/zoom, or screenshot the default and leave? (the zoom-fluency crack)
- Is the **sendable static artifact** (Slack/PDF) the real high-value mode the panel ignored?
- Does **edge-first** evidence (grade on arrows) out-read node chips? (A/B)
- Does the **confidence encoding** stop a hypothesized constraint from reading as proven? (user-test)
