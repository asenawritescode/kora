# Leverage Finder

A light skill for Workshop 4 of the AI + Systems Thinking course. Takes a stock-flow model and a measurable goal, returns the 3 highest-leverage candidate interventions mapped to Donella Meadows' 12 leverage points.

## Files

- `SKILL.md` — the skill for Claude Code (place in `.claude/skills/leverage-finder/` or equivalent)
- `PROMPT.md` — copy-paste version for ChatGPT, Claude.ai, Perplexity (any LLM without Claude Code)
- `test-cases.md` — validation cases (Andrey delegation, Marfa 1k-users, negative goal-gate test)

## Install (Claude Code)

```bash
mkdir -p ~/.claude/skills/leverage-finder/
cp SKILL.md ~/.claude/skills/leverage-finder/
```

Then invoke: `/leverage-finder` in any Claude Code session.

## Use (ChatGPT / Claude.ai / Perplexity)

Open `PROMPT.md`. Copy the entire block under "---". Paste into your LLM. Replace `[YOUR GOAL]` and `[YOUR MODEL]` with your content. Send.

## Design constraints (do not modify)

- **Phase 0 gate.** Skill refuses to proceed without a measurable, time-bound goal. This is the foundational structural move from Sterman + Meadows.
- **Phase 1.5 (structural goal-encoded check).** Goal noun must appear in the model as stock / derived value / readout. If not, Candidate #1 is locked to the encoding fix.
- **Phase 1.6 (semantic stated-vs-enacted check).** Read the structure independent of what the user says; if the system is enacting a different goal, name it and force a choice.
- **Phase 2.5 (slithery-order pre-pass).** A parameter that sets a loop's gain, a balancing setpoint, the system's goal, or who-sees-info is *promoted* to the corresponding higher knob. Ranking uses the promoted knob, not the raw one. Same step demotes knob 9/10/3 when context blocks change.
- **3 candidates, no more.** Forcing the choice is part of the discipline.
- **At least one candidate at promoted-knob ≤5.** The asymmetry lesson. Without this rule, the skill defaults to safe parameter tweaks.
- **Every candidate carries an `Intuitive push` and `Correct push` field.** This is Meadows' opening thesis: people identify the right knob and turn it the wrong way. Surfacing the direction on every candidate is non-negotiable.
- **Knob ≤6 gets "first move this month", not a 2-week test.** High-leverage interventions don't compress into 2-week experiments; forcing them into that box systematically demotes them.
- **Knob 7 default direction is REDUCE gain.** Amplifying a reinforcing loop is the exception, not the default.
- **No invented structure.** Two exceptions only: (a) adding a stock for the goal-encoding candidate, (b) adding an info-flow node for the accountability-gap candidate.
- **Closing line is verbatim and mandatory** — Meadows' own "tentative" / "slithery" disclaimer.

## Anti-patterns this skill prevents

- Marching through all 12 leverage points as a checklist (Meadows refused to call ready, in council)
- Returning leverage analysis on a goalless model (Sterman refused)
- Returning >3 candidates because "the user might want options" (Marfa refused — she said she needs the choice forced)
- Sorting strictly by canonical knob number when a parameter is doing higher-knob work (the slithery-order gap)
- Returning a candidate without saying which direction to push it (the wrong-direction gap — Meadows' actual thesis)
- Defaulting knob-7 candidates to "amplify the loop" (collapse-trap gap)
- Treating the model's stated goal as the system's goal (stated-vs-enacted gap)
