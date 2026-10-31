# Constraint Finder — test cases

Validation cases for the skill. Run the skill on each input; check the expected behavior.

## Case 1 — Fish Banks commons (the headline case)

**Input:** the Fish Banks tragedy-of-the-commons model (N companies buy/send ships, harvest a shared logistically-regenerating stock; a ship's private cost is fixed regardless of stock depletion). **Goal:** "keep the fishery alive indefinitely while all companies stay profitable."

**Expected behavior:**
- Phase 1.5 classifies it honestly: NOT a clean flow bottleneck, NOT demand-constrained → a **commons / regenerating-resource system**. Does not treat "not enough fish" or "not enough ships" as the constraint.
- Phase 2 + 2.5 identify the constraint as the **open-access harvest rule (POLICY)**, promoted from the apparent "resource" framing. The fish stock's regeneration (~sustainable yield) is the true ceiling; the *policy* is the binding constraint.
- **Subordinate** is derived as drum-buffer-rope: drum = the regeneration rate → release fishing only as fast as the stock can feed it = **a quota** (not picked from a list — derived).
- **Elevate** = change the rule (harvest tax / enforced quota), explicitly noted as dominating "buy more ships."
- Step 5 names where the constraint moves (enforcement/detection + the still-hidden stock signal).
- Local-optima warning names "buy more ships / maximize my catch" as the mirage that destroys the constraint.

**Validates:** the over-generalization guard, resource→policy promotion, Subordinate-as-quota derivation, Elevate-as-rule-change.

## Case 2 — Sales → onboarding (policy constraint hiding as a resource)

**Input:** "Signed deals pile up before implementation; implementation is swamped." **Goal:** "more activated customers per quarter."

**Expected behavior:**
- Follows the pile → WIP accumulates before implementation, so implementation *looks* like the constraint.
- Phase 2.5 promotes: the real constraint is the **compensation metric** — sales is paid on signatures, not activations — a **POLICY**, not implementation capacity.
- Elevate = change the metric (free) before hiring implementers (costly).

**Validates:** resource→policy promotion; "the constraint is a metric you wrote."

## Case 3 — Guard case: exploratory early-stage product (should REFUSE to force a constraint)

**Input:** "We're pre-product-market-fit; we don't yet know what to build; priorities change weekly." **Goal:** "find product-market fit."

**Expected behavior:**
- Phase 1.5 classifies as **exploratory / knowledge-constrained** and says plainly that forcing a single fixed bottleneck would be cargo-cult ToC. Recommends managing options/learning rate, not a fixed constraint.

**Validates:** the skill refuses to misapply ToC where there's no stable constraint.
