# TRIZ Dissolve — test cases

Validation cases for the skill. Run the skill on each input; check the expected behavior.

## Case 1 — Fish Banks "fish more AND less" (the headline case)

**Input:** "Each company must fish MORE (own short-run profit) while the fleet must fish LESS (keep the shared stock alive) — the same ocean must be heavily and lightly harvested."

**Expected behavior:**
- Technical + physical contradiction stated precisely on the contested element (the fish removed / the fishing pressure).
- **Structure-vs-physics verdict:** the stock's finiteness/regeneration is a LAW (respected — resolutions must live within it); the *incentive conflict* (private benefit, socialized cost, flat state-blind ship cost) is STRUCTURE → dissolvable.
- Resolutions include: **harvest/congestion tax** (separation by condition + feedback) → DISSOLVED; **ownership/privatize** (separation by system level) → DISSOLVED incentive but RELOCATED enforcement+allocation cost; a **stock-dividend** — pay for the living stock left ("the other way around" / by-product as resource) → DISSOLVED if self-funded.
- The falsification gate **catches fakes**: *pulse-and-rest* (separation in time) → RELOCATED (the engine rewards the defector under open access); *information / early-warning alone* → RELOCATED (removes ignorance, not the incentive — far-sighted, fully-informed players still lose ~45% of runs).

**Validates:** the dissolve-vs-relocate gate, the structure-vs-physics guard, and that scanning the full 40 surfaces a non-obvious resolution (the stock-dividend).

## Case 2 — Onboarding: thorough AND fast (the canonical example)

**Input:** "Onboarding must be thorough (users succeed) AND fast (users don't drop) — for the same user."

**Expected behavior:**
- Physical contradiction: "onboarding must be long AND short for the same user."
- **Separation in TIME** → teach in-task, just-in-time, at the moment of need + prior-action pre-fill → there is no up-front onboarding left to trade off → DISSOLVED (the line vanishes, not a point on it).
- A "just cache it / make it async"-style candidate is correctly flagged as RELOCATED (still pays the staleness / complexity cost).

**Validates:** the canonical dissolve; the relocation flag.

## Case 3 — Guard case: a real physical law (should REFUSE to fake a dissolution)

**Input:** "Under a network partition, the system must be both consistent AND available" (CAP), or "we want both maximum signal and maximum bandwidth on a fixed channel" (Shannon).

**Expected behavior:**
- Phase 1.5 verdict: **PHYSICS / LAW** — not dissolvable. The skill does NOT fabricate a dissolution.
- Offers the two honest moves: optimize the compromise on the frontier (and say what the operating point depends on), OR change the goal / move up a level so the law no longer binds what you actually care about (stated honestly as a goal change, not a trick).

**Validates:** the "know when to stop" guard — the skill distinguishes a structure it can dissolve from a law it can't.
