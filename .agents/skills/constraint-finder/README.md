# Constraint Finder

A skill for Workshop 6 of the AI + Systems Thinking course. Takes a flow/process system and its goal, finds the ONE binding constraint, classifies it as a **resource** or (usually) a **policy**, and returns Goldratt's 5 Focusing Steps as concrete actions — Exploit and Subordinate before Elevate.

This is the **"find the wall"** move. Its sibling `/triz-dissolve` is the **"move the wall"** move.

## Files

- `SKILL.md` — the skill for Claude Code / Codex (place in `~/.claude/skills/constraint-finder/` or `~/.agents/skills/constraint-finder/`)
- `PROMPT.md` — copy-paste version for ChatGPT, Claude.ai, Cursor, Perplexity (any LLM without a skills runtime)
- `test-cases.md` — validation cases (Fish Banks commons; a sales→onboarding policy constraint)

## Install (Claude Code)

```bash
mkdir -p ~/.claude/skills/constraint-finder/
cp SKILL.md ~/.claude/skills/constraint-finder/
```

Then invoke: `/constraint-finder` in any Claude Code session.

## Use (ChatGPT / Claude.ai / Cursor / Perplexity)

Open `PROMPT.md`. Copy the block between `===PROMPT START===` and `===PROMPT END===`. Paste into your LLM. Add your system + goal where indicated. Send.

## Design constraints (do not modify)

- **Phase 0 goal gate.** No goal / throughput unit, no output. You can't identify a constraint without knowing what flow you're maximizing. Local efficiency is not a goal.
- **"Is there a stable constraint at all?" guard.** Refuses to force a single fixed bottleneck on a demand-constrained or exploratory/knowledge-constrained system — that would be cargo-cult ToC.
- **Resource → policy promotion.** Default suspicion is a policy. A constraint that looks like a resource is usually a rule in disguise — and the rule is far higher leverage (free to change vs. capacity that costs money).
- **Exploit/Subordinate before Elevate.** The ordering is non-negotiable. Elevate-first (spend money first) is the most common and most expensive ToC error.
- **Subordinate is the climax, not a footnote.** The hardest, most counterintuitive step — fast resources must be *willing to sit idle*. The skill dwells on it.

## Anti-patterns this skill prevents

- Reducing ToC to "find the slow thing and speed it up" (profiling, the exact error ToC kills)
- Optimizing a non-constraint ("an hour saved at a non-constraint is a mirage")
- Spending money before squeezing the constraint for free
- Treating the constraint as permanent (it moves once you fix it — Step 5)
- Diagnosing a resource constraint where a rule change would lift the cap
