# Answer-kinds & grading - the core of the rigor

This is what separates a Why Tree from a confident-sounding guess. **Every node gets three tags: an answer-kind, a confidence grade, and a citation.** The answer-kind tells the reader *what kind of thing they're trusting* before they decide how much to trust it.

## The seven answer-kinds

| Kind | Chip | Means | Test before you assign it |
|------|------|-------|---------------------------|
| **MEASURED** | green | A hard number from real data, with an n. "64% of new workspaces add a 2nd member (n=820)." | Is there an actual count/rate behind it? What's the n and the window? **And is the pipeline clean — see the validation ladder below. A number is not yet a fact.** |
| **INSTANCE** | amber | A specific observed example, n=1-few. "One new workspace was created with no teammate invited." | Is this one case (or a handful), not a rate? Don't generalize it. |
| **EXTERNAL** | blue | A third-party / industry fact not about this system. "Gmail's complaint ceiling is 0.3%." | Is it from outside the system, and is the source named? |
| **CLAIM** | red | A stakeholder assertion, unverified. "~20 signups/day." | Did a person say it without data? Flag contradictions (e.g. another source implies ~2/day = 10x gap). |
| **INFERENCE** | purple | Logic built on measured premises. "92% never invite a teammate, so the surface is mis-positioned." | Are the premises MEASURED? Is the logical step valid, or is there a hidden third factor? |
| **HYPOTHESIS** | gold-dashed | A testable proposition not yet tested. "Users finish the invite step in the UI by preference." | Can you name the exact test that would settle it? If not, it's not even a hypothesis. |
| **FRAMING** | grey | A restatement of the problem itself, a lens. The apex is always FRAMING. | Is this a way of *seeing* the problem rather than a cause? |

**The decisive insight:** the nodes that *locate the constraint* are often the HYPOTHESIS ones - exactly the part that's unmeasured. A tree that's "~45% MEASURED" can still be shallow at the decision point if the 20% that would pin the constraint is all HYPOTHESIS. The census (below) exists to catch this.

## The MEASURED validation ladder (a number is not yet a fact)

MEASURED certifies that a number **exists**. It does NOT certify that the number **means what the node claims** - and that gap is the skill's field-proven failure mode: a real business run shipped three MEASURED/high-confidence nodes that were all pipeline artifacts (bots in the denominator; an API metric whose *definition* didn't match the claim; a seasonal swing read as a trend), and 4 of 6 follow-up iterations went to debunking by hand what the tree had called "facts". "Can I compute it?" and "is the pipeline clean?" are different questions - so every MEASURED node also carries a **`validation` state**:

| State | Means | Consequences |
|-------|-------|--------------|
| **`raw`** | Computed from a source whose pipeline has NOT been checked. The default for any fresh query. | **Grade caps at Mod.** May NOT serve as the constraint's `loadBearing` support; if it ends up decisive, `verdictStatus` = `map`. |
| **`validated`** | The source passed the pipeline-cleanliness checklist below, or is a **curated/canonical** source (the dashboard / P&L the business already trusts) whose definition matches the claim. | Eligible for Strong. |
| **`triangulated`** | Two INDEPENDENT pipelines computed the number and agree within tolerance. | The strongest state - what "high confidence" should actually mean. |
| **`contested`** | Two sources disagree beyond tolerance (worst case: opposite signs). | Grade drops to Weak; **BOTH values + BOTH cites stay visible in the node**; it cannot support a constraint; the reconciliation auto-becomes a cheapest-test / data-request line. The discrepancy is itself a finding - usually a definition mismatch on one side. |

Field example of `contested` done right: AOV from a raw price-range query said **−27%** while the finance dashboard said **+12%** - opposite signs from two pipelines. That node must ship as CONTESTED with both numbers, never as a MEASURED fact wearing the same green chip as a verified one.

### The pipeline-cleanliness checklist (what `validated` means)

Cleanliness is a property of the **source**, not of each node - run this ONCE per source in Phase 1, then nodes inherit the answer:

1. **Denominator pollution** - who is in the base? Bots, internal/test traffic, staff accounts, dead SKUs, deleted users?
2. **Definition mismatch** - does the source's metric definition match the claim's *meaning*? (placed vs paid orders, sessions vs users, gross vs net, list vs realized price, event-time vs ingestion-time)
3. **Window & seasonality** - is the window representative? Is any Δ-claim normalized (see the Δ-over-time rule)?
4. **Attribution** - does the join/attribution model actually connect *this cause* to *this effect*, or just co-locate them?
5. **Survivorship in the extract** - does the export pre-filter (active-only, completed-only, non-null-only) in a way that biases the number?
6. **Computation traps** - the query itself can manufacture the artifact: **join explosion** (a many-to-many join silently multiplies rows — check row counts before/after every join; count entities with `COUNT(DISTINCT id)`), **average-of-averages** (unweighted means over unequal groups), **partial-vs-full period** ("January is down" — checked on the 20th).

### The Δ-over-time rule (levels are measured; trends are claims)

A LEVEL can be MEASURED. A TREND is a claim about two levels, and seasonality is the hidden third factor. **Any Δ-over-time claim ("dropped Jan→Jun", "H1 vs H2") without a YoY comparison or seasonal normalization is at best INFERENCE/Weak** - whatever the grades of the underlying numbers. (Field run: two false trends shipped as facts; both evaporated under the year-over-year slice.)

Three more ways a Δ lies, each a one-line check:
- **Denominator shifting** - the metric's *definition* changed between the compared periods ("eligible users" recounted, "active" redefined). A Δ-claim requires definition constancy across both endpoints.
- **Mix-shift (Simpson)** - the aggregate moves while every segment moves the other way, because the mix changed. A causal Δ-claim should survive segmentation by the obvious splits.
- **Noise floor** - a small Δ on a small n is not a trend ("−3% at n=40"). If the change is within the noise for its sample size, the trend claim is Weak no matter how real the two numbers are.

## Confidence grade (orthogonal to kind)

Independent of *what kind* of evidence it is, how strong is it?

- **Strong** - robust within its kind (large n, clean source, tight logic).
- **Mod** - real but limited (small n, single window, one source).
- **Weak** - thin, suggestive only.

A node can be `MEASURED / Weak` (real number, tiny n) or `EXTERNAL / Strong` (well-established third-party fact). State both. A common honest tag is `CLAIM / Strong-that-it's-unverified` - we're sure someone said it, unsure it's true.

## Role badges (assigned during convergence, Phase 2 step 5-6)

| Role | Means |
|------|-------|
| **ROOT** | A point where branches converge - a root cause (label R0/R1...). |
| **CONSTRAINT?** | A candidate for THE system constraint. Exactly one should end up as THE constraint. |
| **CONSTRAINT** | THE located system constraint — candidates resolve to exactly one of these at converge. |
| **LEVER** | A fixable point with good leverage. |
| **NEGATIVE** | A fix that would backfire - kept in the tree as a warning. |
| **REFUTED** | Killed by the adversarial pass - kept visible with the killing evidence. |
| **DOWNGRADED** | Survived but demoted (e.g. "blocker -> friction reducer") with the evidence that demoted it. |
| **critic-added** | Surfaced by the refute/critic pass, not the original fan-out. |

## Citation rules

- Every non-FRAMING node names its source: a file+section (`What_We_Learned §5`), a query result (`n=6 sends, 7-day window`), a URL, or a named stakeholder.
- MEASURED nodes must state the **n and the window** - "45% (n=820, Mar 3-9)", never a bare percentage.
- MEASURED nodes also name their **source-registry id** (Phase 1) and their **validation state**; a `raw` node must *say* it's raw. A `contested` node keeps **both** values and **both** cites - never just the convenient one.
- If a node's only source is one person, it's a CLAIM, not MEASURED - no matter how confident they sounded.
- A HYPOTHESIS with no nameable test is not allowed - either find the test or drop the node.
- **Measurable-in-hand rule (decisive):** a node is NOT a HYPOTHESIS or CLAIM if one query/read over evidence you have ALREADY pulled would settle it. Run the check and grade it MEASURED. Distinguish two flavors of "unmeasured": *measurable-in-hand* (you just didn't look — go look, now) vs *genuinely-external* (needs new data you don't have). Only the latter may stay HYPOTHESIS and become a cheapest-test; the former is a research gap, not a real unknown. (Real example: "why activators stop before the 50-seat wall" was left HYPOTHESIS when the items-per-account *distribution* — which answers it — was one `GROUP BY` away in a table already queried.)
- **Threshold needs its distribution:** a node that makes a causal claim about why people don't cross a threshold (≥N, hit-the-wall) needs the **distribution** of the underlying quantity, not just the threshold count. Without it, the claim is at best INFERENCE/Weak — "83% never reached 50" cannot tell "satisfied at 48" from "bounced at 5."

## The evidence census (Phase 4)

Tally the answer-kind mix and write the honest bottom line. Template:

> ~X% MEASURED (but note the window/n caveat) · ~Y% INFERENCE · ~Z% HYPOTHESIS - **and the constraint-locating nodes are in the HYPOTHESIS bucket.** So: broad but shallow at the decision point. Headline = "where to look," not "here's the verdict." The #1 cheapest-test targets exactly those HYPOTHESIS nodes.

If instead the decisive nodes are MEASURED/Strong, say so - then the tree *is* a verdict, and the recommendation can be "act," not "test first." But **MEASURED/Strong requires `validated` or `triangulated`** - a census that counts `raw`/`contested` nodes as measured overstates the ground truth; call them out separately in the bottom line ("~40% measured, of which half is raw/uncontested-unchecked").
