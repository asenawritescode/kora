# TRIZ Dissolve

A skill for Workshop 6 of the AI + Systems Thinking course. Takes a trade-off ("improving X worsens Y") and **eliminates the contradiction** instead of balancing it — using TRIZ: physical-contradiction reframing, the Ideal Final Result, the separation principles, and the full set of 40 inventive principles, with a falsification test on every candidate.

This is the **"move the wall"** move. Its sibling `/constraint-finder` is the **"find the wall"** move.

## Files

- `SKILL.md` — the skill for Claude Code / Codex (place in `~/.claude/skills/triz-dissolve/` or `~/.agents/skills/triz-dissolve/`)
- `PROMPT.md` — copy-paste version for ChatGPT, Claude.ai, Cursor, Perplexity
- `test-cases.md` — validation cases (Fish Banks "fish more AND less"; onboarding thorough-vs-fast; a real-physics case it must refuse to fake)

## Install (Claude Code)

```bash
mkdir -p ~/.claude/skills/triz-dissolve/
cp SKILL.md ~/.claude/skills/triz-dissolve/
```

Then invoke: `/triz-dissolve` in any Claude Code session.

## Use (ChatGPT / Claude.ai / Cursor / Perplexity)

Open `PROMPT.md`. Copy the block between `===PROMPT START===` and `===PROMPT END===`. Paste into your LLM. Add your trade-off where indicated. Send.

## What the skill is for (its focus)

The LLM already knows TRIZ — the 40 principles, the 4 separations, IFR, ideality. So the skill does **not** re-teach it. It does two things:

1. **Enforces the discipline the LLM skips unprompted.** Left alone, an LLM degrades TRIZ into brainstorming and defaults to *compromise*. The phases force the real method: physical contradiction → IFR → structure-vs-physics → dissolve-vs-relocate test.
2. **Makes the LLM use the full toolkit.** "You ARE the contradiction matrix" — scan all 40 principles + the 4 separations + resource analysis, not just the obvious separation. This is where non-obvious dissolutions come from.

It does NOT reprint the 40 principles or rely on exact contradiction-matrix cell recall (which is hallucination-prone).

## Design constraints (do not modify)

- **Refuse compromise.** "Find the optimal balance / tune the ratio / a point on the curve" is never an answer — it's the failure mode the skill exists to break.
- **Structure-vs-physics guard.** Before dissolving, classify: dissolvable structure (proceed) or irreducible physical law (don't fake it — optimize the honest compromise or change the goal/level).
- **Dissolve-vs-relocate test on every resolution.** Did the cost leave the system, or just move (to another team, a later time, the user, tech debt)? A relocation is a disguised compromise, flagged — not a dissolution.
- **Physical-contradiction reframing is mandatory.** The technical contradiction alone doesn't unlock the separations.

## Anti-patterns this skill prevents

- Defaulting to compromise (the unprompted-LLM reflex)
- Treating every trade-off as dissolvable (some are real physics)
- Letting a cost-relocation masquerade as a dissolution
- Naming an inventive principle without showing how it actually separates the demands
- A physical contradiction so vague it fits anything (→ fake resolutions)
