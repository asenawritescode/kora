# Methodology - Goldratt's Current-Reality Tree, in practice

The Why Tree is Eliyahu Goldratt's **Current-Reality Tree (CRT)** from the Theory of Constraints, adapted for AI-agent execution. The CRT is a cause-and-effect diagram that traces a system's **undesirable effects (UDEs)** down through layers of cause to the **few root causes** - and, ideally, the **single core constraint** that produces most of the UDEs.

## The core beliefs (why the method is shaped this way)

1. **There is one constraint.** In any system, one thing limits throughput more than everything else. Fixing non-constraints feels productive and changes nothing. The tree exists to *find the one*, not to list many. (Goldratt: "There is always one. Everything else is noise.")
2. **Effects share causes.** Surface symptoms that look unrelated usually trace to a common root. Branches that look independent at the top should *converge* as you go down. If they don't converge, you haven't gone deep enough.
3. **Cause must be sufficient, not just present.** "X correlates with Y" is not a CRT arrow. The arrow means *"if X, then Y, and X is enough to produce Y here."* This is what the CLR audit polices.
4. **The constraint is often a policy, not a thing.** The most common real constraint is a rule, a metric, an incentive, or an unowned responsibility - not a missing tool or feature. Teams reflexively reach for tooling fixes because policy fixes are political. The tree should call this out when it sees it.

## The WHY-branch logic (how the tree is built)

- **Apex (the UDE / gap-vs-goal).** One undesirable effect, stated as the gap between reality and a goal. It is a FRAMING node - a hypothesis about what the real problem is. The tree may reframe it.
- **Branch out, then down.** First enumerate *all* the candidate "why"s across independent lenses (don't deepen the first one). Then, for each, recursively ask **"why does this happen?"** until you hit **bedrock** - a cause you can't or won't go beneath (a market fact, a deliberate policy, a law of the domain). This is **loop-until-dry**: stop a branch only when another "why" yields nothing new.
- **Bucket by stage.** For journey/funnel problems, group branches by stage (Arrive -> Connect -> Activate -> Prepare -> Send -> Complete, or your domain's equivalent) plus a **cross-cutting** group for things that act at every stage (ownership, incentives, metrics, economics). The cross-cutting group is where the real constraint usually hides.

## Convergence - collapsing to the root(s) and the ONE constraint

After branches are deepened and graded:
1. **Merge** branches that bottom out at the same cause into a labeled **root** (R0, R1, ...).
2. **Locate the constraint**: ask *"which single root, if removed, would unblock the most other branches?"* That's THE system constraint. Mark it unmistakably. Everything else is a lever or noise relative to it.
3. **Separate located from unlocated.** It's common and honest to find that the **system constraint is located** (e.g. "no owner + goal-conflict") while the **mechanism/funnel constraint is still unlocated** (needs one query). Say both.

## The Evaporating Cloud (when the constraint is a conflict)

If the core constraint is a **goal-conflict** ("we must protect the dying revenue model" vs "we must grow the new one"), it's an **Evaporating Cloud** - a conflict that feels unbreakable because both sides serve a shared higher goal. Break it by surfacing the false assumption under one arrow. (Source-case example: "growing agent-completions cannibalizes per-seat revenue" dissolved once you see per-seat is *already* dying - so completions are the *replacement* metric, not a cannibal. The conflict evaporates.)

## Evaluating an idea against the tree (CRT → FRT: the injection test)

Goldratt's sequence for solutions: after the Current-Reality Tree comes the **Future-Reality Tree** — a proposed fix is an **injection**, tested by asking which CRT nodes it flips and whether the constraint actually moves. The corollary most people skip: **an idea is never judged against the symptom, only against the tree.** "Onboarding is slow; my product speeds up knowledge access" pattern-matches at the symptom level and can still miss the constraint entirely (the field case: the constraint was that nobody *owned* the onboarding process — a policy node no knowledge tool touches).

Idea Audit mode operationalizes this: diagnose the problem the idea *presupposes* (with the idea quarantined, so the tree isn't built to flatter the pitch), then map the idea onto the finished tree — does it dissolve the constraint, relieve a symptom, miss, or backfire (a negative branch, run forward like any fix)? Two idea-specific reservations extend the CLR:

- **The absorbed-pain reservation.** A chronic UDE the system has compensated around for years is part of its equilibrium: the cost is budgeted, the workarounds are staffed, and willingness-to-pay for removing it approaches zero even when the pain is real and everywhere — *widespread ≠ payable*. This is still a graded claim about buyer behavior, not a verdict: attach the test (pre-sell, budget-line check, adjacent purchases).
- **The n=1 reservation.** A CRT diagnoses ONE system. The idea's market is the set of systems where the located constraint — and the pain's acuteness — actually holds, so the honest output is a **segment fork** ("dies where X, may live where Y"), which turns a kill into ICP selection.

And always the **redirect**: the tree's reframed problem is the alternative idea space — what *would* dissolve the constraint is often a better product than the one on trial.

## CLR audit - Categories of Legitimate Reservation (Phase 4)

Goldratt's checklist for validating cause-effect logic. Apply to every load-bearing arrow:
- **Clarity** - is the statement unambiguous?
- **Entity existence** - is the cause and effect each a real, complete statement?
- **Causality existence** - does the cause genuinely lead to the effect (not just co-occur)?
- **Cause sufficiency** - is the cause *enough* on its own, or is a stated factor missing?
- **Additional cause** - is there another independent cause of the same effect?
- **Cause-effect reversal** - did you get the arrow backwards (is the "effect" actually the cause)?
- **Predicted-effect existence** - if the cause is real, what *else* should be observable? Go look for it.

## Negative branches (fixes that backfire)

Before recommending action, run each candidate fix forward: *"if we do this, what undesirable new effect appears?"* A fix with a severe negative branch is a trap. Keep these in the tree as **NEGATIVE** nodes - they're often more valuable than the positive recommendations because they stop expensive mistakes. (Examples: auto-inviting a user's whole contact list to boost the metric triggers spam complaints and a deliverability cliff; keystoning a fix on a behavior the data says almost no one does; harder-optimizing the vanity number the real constraint already games.)

## Cheapest tests (the bridge from tree to action)

The tree's most actionable output is *"what one read would most change the conclusion?"* Rank candidate tests by **(how much it moves the verdict) ÷ (cost to obtain)**. The winner is usually a single query or a small experiment that adjudicates several HYPOTHESIS nodes at once. The recommendation when evidence is thin is almost always **"run the cheapest fork-deciding test + name an owner," not "analyze more."**

## What "done" looks like

A finished Why Tree: every node graded and cited · load-bearing branches refuted-or-survived · branches converged to 3-5 roots and ONE constraint · negative branches named · 1-3 cheapest tests ranked · an honest census saying whether it's a verdict or a map. The diagnosis feeds a one-page decision doc; the tree is not the deliverable's end - the located constraint + the cheapest test are.
