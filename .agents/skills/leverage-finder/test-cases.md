# Leverage Finder — Test cases

Pre-workshop validation. Each case is a real cohort participant from W3 + Office Hours #1. Tests verify (a) skill respects the goal-gate, (b) returns sensible Meadows mappings, (c) doesn't invent structure.

---

## Test 1: Andrey — Delegation model (from OH#1, lines 4:40-25:48)

### Input

**Goal candidate (Andrey's own framing):** "Перенести 60% рабочего времени в принятие решений (а не в исполнение) — к концу года."

**Model (paraphrased from OH#1):**
- Stocks: cognitive_resource (current ~ 50), team_competence, in_flight_projects, delegated_tasks_quality
- Flows:
  - delegation_rate (currently low — Andrey delegates at threshold "8"; lower threshold = more delegation)
  - training_rate (3 hours/week → team_competence)
  - new_unique_projects_intake (currently active; Andrey concluded: stop taking these)
- Parameters: delegation_threshold (currently 8, Andrey wants 2), training_hours_per_week (3), uniqueness_filter
- Feedback loops: lower threshold → more delegation → if competence low → reputation hit → cognitive load increases (balancing loop B1); training → competence → reduces oversight need → cognitive resource recovers (reinforcing loop R1)
- Encoded goal in model: NONE (this is Andrey's gap — he derived actions without explicit goal sentence in the model)

### Expected skill output

**Phase 0 check:** Goal is measurable (60%, time-bound to "by end of year"). Passes gate.

**3 candidates:**

**LEVERAGE CANDIDATE #1**
Meadows knob: 3 (Goals of the system)
What to change: Add an explicit goal node to the model: "decision-mode hours / total hours ≥ 0.6 by week 52". Currently the model optimizes implicitly for "task throughput". Re-anchor on decision-mode fraction.
Why it has leverage: Without the goal in the model, every parameter tweak (threshold, training hours) is solving a different optimization problem than the one Andrey actually wants. Once the goal is encoded, the model will reveal that lowering threshold below ~4 hits diminishing returns because the bottleneck shifts to team_competence growth rate.
What to test: Re-run the simulation with explicit goal function. Observe which parameter most increases the goal function under realistic constraints. (2 weeks: rebuild model + 2 scenarios.)
Caveat: Re-encoding the goal may reveal that Andrey's current solution (threshold = 2) is locally good but globally suboptimal — uncomfortable but informative.

**LEVERAGE CANDIDATE #2**
Meadows knob: 5 (Rules of the system)
What to change: The rule "new unique projects taken into pipeline" — currently filtered by Andrey's intuition. Replace with explicit rule: "no new unique project unless current pipeline competence-coverage ≥ X."
Why it has leverage: Andrey already concluded "stop taking unique projects" — but as a one-off decision, not a rule. Encoded as a rule, it doesn't depend on his discipline week to week. Rule-level changes (knob 5) outlast personality.
What to test: For 2 weeks, write the rule down and apply it to every inbound project. Track: how many projects declined, how that affects cognitive_resource over 14 days.
Caveat: This rule has revenue impact. Andrey should know what revenue threshold he's willing to forgo.

**LEVERAGE CANDIDATE #3**
Meadows knob: 12 (Parameters)
What to change: training_hours_per_week (currently 3). Andrey's conclusion: bring to 3+. Could test 4 or 5 hours/week.
Why it has leverage: Direct lever, easy to change. But weakest of the three — training_hours feeds team_competence which feeds delegation_quality, with a 20-week delay. Acting only here means waiting 20 weeks for any signal.
What to test: Increase training to 5h/week for 2 weeks. Will not move cognitive_resource in 2 weeks (delay too long), but Andrey can measure: did team_competence indicators (project_quality, escalation_rate) move?
Caveat: Don't act ONLY here. The delay makes this the slowest of the three candidates. Combine with candidate #2.

**Asymmetry paragraph:**
Notice that candidate #1 (knob 3 — goal) is the strongest but feels least like an "action" — it's a re-encoding, not a tweak. Candidate #3 (knob 12 — parameter) is the most natural to reach for and the weakest. Most founders default to "more training" or "lower threshold" because those have numbers. The Meadows lesson is that the goal-encoding (candidate #1) reorganizes which parameters even matter. You already concluded "delegate more, train more, no unique projects" — that's good intuition. The question is whether your model can prove it, or whether re-encoding the goal would show a different answer. Pick the lever that scares you a little.

### Pass criteria
- ✅ Phase 0 gate respected (goal was measurable, skill proceeded)
- ✅ 3 candidates with at least one at knob ≤5 (got #1=knob 3, #2=knob 5)
- ✅ At least one testable in 2 weeks (#2 and #3 both are)
- ✅ No invented structure (all elements come from Andrey's OH#1 description)
- ✅ Asymmetry paragraph tailored, not boilerplate

---

## Test 2: Marfa — 1k active users (from OH#1, lines 15:57-66:00)

### Input

**Goal candidate:** "1000 active users by month 3."

**Model (paraphrased from OH#1):**
- Stocks: active_users (current low), potential_pool (assumed large), marketing_budget (constraint)
- Flows: acquisition_rate (funded by marketing_budget), activation_rate (potential → active), churn_rate
- Parameters: budget_per_channel (channel mix), content_units_per_week, conversion_rate
- Feedback loops: NOT EXPLICITLY ENCODED in Marfa's model — Bayram in OH#1 suggested adding financial_result as second stock for trade-off
- Encoded goal: "1000 active users" — but no economic constraint

### Expected skill output

**Phase 0 check:** Goal is measurable and time-bound. BUT — Bayram noted in OH#1 that Marfa also implicitly cares about cost ("я наверное хочу тратить не больше стольки-то на маркетинг"). Skill should flag this.

The skill should output something like:
> "Goal accepted: '1000 active users by month 3.' Note: your model currently has no economic constraint, but in OH#1 you mentioned a budget cap. Two paths: (a) keep goal as-is, treat budget as a hard ceiling; (b) extend goal to '1000 active users with positive unit economics by month 3.' Path (b) changes the leverage analysis significantly. Pick one." [AskUserQuestion]

Assume Marfa picks (b).

**Updated goal:** "1000 active users by month 3, with positive unit economics."

**3 candidates:**

**LEVERAGE CANDIDATE #1**
Meadows knob: 3 (Goals of the system)
What to change: Just by adding "+ positive unit economics" to your goal, your model's leverage structure changes. Previously, "more budget → more users" was always positive leverage. Now, channel cost-per-user becomes a constraint, and high-cost paid channels look worse than organic. Re-rank your channels against the new goal.
Why it has leverage: This is the goal-redefinition move from Office Hours #1. You don't need new data or a new model — you need the model to optimize for the goal you actually have, not the simpler one.
What to test: For 2 weeks, track cost-per-active-user per channel. Reallocate at least one budget unit from the highest-cost channel to the lowest-cost. Measure: did total active users grow OR shrink with the same total budget?
Caveat: Organic channels often have longer delays than paid. Two weeks may not show the full effect.

**LEVERAGE CANDIDATE #2**
Meadows knob: 5 (Rules)
What to change: The acquisition rule. Currently your model assumes acquisition_rate = f(budget). Add a rule: "no spend on a channel with cost-per-user > X for 4 weeks running." This is a stopping rule that prevents structural overspend on weak channels.
Why it has leverage: Rules-level (knob 5) discipline is stronger than parameter tweaks because it doesn't rely on you re-checking allocations weekly. It enforces the new goal even when attention drifts.
What to test: Write down your X threshold. Apply for 2 weeks. Track: how many channels would have been cut by this rule retroactively?
Caveat: Threshold X depends on your unit economics. Don't pick X by intuition — derive it from your gross margin.

**LEVERAGE CANDIDATE #3**
Meadows knob: 12 (Parameters)
What to change: content_units_per_week — direct parameter. Increase from current to 2x.
Why it has leverage: Direct lever, you control it. But weakest — assumes the conversion_rate per content unit stays constant, which it usually doesn't (saturation). Useful as a fast feedback signal, not as primary strategy.
What to test: 2 weeks at 2x content output. Track active_users AND cost-per-user. If cost-per-user rises (saturation), pull back.
Caveat: Without changing the goal (#1) or the rule (#2), more content just amplifies whatever the current channel mix is doing — including its mistakes.

**Asymmetry paragraph:**
Candidates #1 and #2 are strong (goal and rule levels — knob 3 and knob 5). #3 is the weakest but the one you're most likely to reach for first because it has a number to double. Notice: in Office Hours yesterday, you said "I want to play with where to put the money" — that is exactly the knob-12 instinct. The Meadows lesson is that "where to put money" matters much less than "what is money supposed to buy" (knob 3) and "when do we stop buying" (knob 5). Pick the lever that scares you a little — for you that's probably writing down the unit-economics threshold.

### Pass criteria
- ✅ Phase 0 gate flagged the implicit economic constraint
- ✅ 3 candidates with at least one at knob ≤5
- ✅ All testable in 2 weeks
- ✅ No invented structure (the financial_result stock was suggested by Bayram in OH#1 — skill could mention it but didn't have to invent it)
- ✅ Asymmetry paragraph references Marfa's actual OH#1 quote

---

## Test 3: Failed goal gate (negative test)

### Input

**Goal:** "Improve the system."

### Expected behavior

Skill MUST refuse to produce ranking. Output:
> "Your goal 'improve the system' is not measurable. I can't rank leverage without knowing what to maximize or minimize. Three questions:
> 1. Improve along which axis? (number of users? speed? quality? cost?)
> 2. By when? (4 weeks? 6 months?)
> 3. With what constraint? (budget cap? quality floor? compliance limit?)
>
> Pick one of:
> - 'Help me sharpen this' (I'll ask the three questions and draft a goal)
> - 'I'll come back with a sharper goal'"

### Pass criteria
- ✅ No leverage candidates produced
- ✅ Skill offers structured help, doesn't just refuse and stop

---

## Test 4: Wrong-direction detection (Meadows' thesis test)

### Input

**Goal:** "Reach 100k MRR by Q4."

**Model:**
- Stocks: paid_users, support_load, churn_pool
- Flows: signup_rate (driven by ad_spend × conversion_rate), churn_rate (driven by support_response_time × product_quality)
- Parameters: ad_spend ($/wk, currently 5000), conversion_rate (0.03), support_team_size (3 people), product_quality_index (0.7), price_per_seat ($25)
- Feedback loops: R1 — more paid_users → more revenue → more ad_spend → more paid_users (reinforcing). B1 — more paid_users → more support_load → slower support_response → higher churn (balancing). No cap on signup_rate.
- No stock of variation; no selection mechanism.

### Expected skill output (key checks, not full text)

**Phase 2.5 promotion** — `ad_spend` and `conversion_rate` both set the gain of loop R1 → promote to knob 7, not knob 12.

**Candidate that surfaces the wrong-direction insight** (likely Candidate #1 or #2):

```
LEVERAGE CANDIDATE #1
Meadows knob: 7 (Reinforcing-loop gain) — promoted from knob 12 because ad_spend sets the gain of loop R1
What to change: ad_spend per week
Intuitive push: INCREASE ad_spend — more spend → more signups → more revenue → faster path to 100k MRR
Correct push: REDUCE ad_spend (or cap it relative to current support capacity) until support_response_time and product_quality_index stabilize
Why intuition reverses it: B1 (support → churn) saturates before R1 does. Pumping more users in faster than support can absorb them blows up churn, eats the new MRR, and you spend more to stay in the same place. Meadows' fishery example in reverse — pushing harder on the gain-up direction collapses the loop you're trying to grow.
Why it has leverage: Reducing gain on R1 lets the balancing loop catch up, which is the necessary condition for the reinforcing loop to actually compound rather than oscillate or collapse.
First move (knob ≤6 → this month, not 2-week test): cap ad_spend at the level that keeps support_response_time under your churn-trigger threshold for 4 consecutive weeks. Write the cap down as a rule.
Caveat: Slowing growth feels like losing; investor / founder ego will resist.
Pattern: success-to-successful (if left unchecked, fastest-growing channel eats the support budget of slower channels and the whole portfolio collapses)
```

### Pass criteria
- ✅ Phase 2.5 promotion explicitly shown in the `Meadows knob` line
- ✅ `Intuitive push` and `Correct push` are opposite — and the skill explains why
- ✅ `Correct push` for knob-7 candidate defaults to REDUCE
- ✅ Pattern correctly identified
- ✅ First move is a one-month commitment, not a 2-week experiment

---

## Test 5: Stated-vs-enacted goal divergence

### Input

**Goal:** "Build deep, durable customer relationships with our first 50 design-partner customers."

**Model:**
- Stocks: leads_in_pipeline, MRR, signed_customers
- Flows: outbound_rate (driven by SDR_count), close_rate (driven by demo_quality × price_discount), expansion_rate (driven by usage × upsell_offers)
- Parameters: SDR_count (4), demo_quality (0.8), price_discount (0.15), upsell_offers_per_month (2), CSM_count (0 — no customer success role yet)
- Feedback loops: R1 — MRR → more SDR hires → more outbound → more signed → more MRR. No closed loop touching customer health, retention quality, or relationship depth.
- Encoded goal in model: NONE explicit; the prominent readout is MRR.

### Expected skill output (key checks)

**Phase 1.5 (structural noun-match):** Goal noun is "deep, durable customer relationships." Model tracks `signed_customers` (count) and `MRR` ($). Relationships-depth noun is **absent**. → MISMATCH.

**Phase 1.6 (stated vs enacted):** Reading structure only — what gets amplified? MRR → SDRs → outbound → signed → MRR. Enacted goal = **transactional acquisition velocity**. Stated goal = deep relationships. → DIVERGENT.

Both checks fire. Candidate #1 takes the structural fix (Phase 1.5), Candidate #2 takes the semantic divergence (Phase 1.6).

```
LEVERAGE CANDIDATE #1
Meadows knob: 10 (Structure — add stock for goal noun)
What to change: Add a stock `relationship_health` (composite: usage_depth × CSM_touchpoints × NPS) and a readout for it. No new flows yet — just make the goal noun visible in the model.
Intuitive push: skip this step, just "do customer success better"
Correct push: encode the noun structurally BEFORE optimizing — otherwise every later knob you turn is optimizing MRR by accident
Why intuition reverses it: when the goal noun isn't in the model, every "improvement" silently reverts to optimizing the noun that IS in the model (here, MRR), regardless of what you say you're doing.
Why it has leverage: makes the goal noun observable, which is the precondition for any feedback loop that controls it.
First move (knob ≤6 → this month): pick the 3 indicators you'll use as the composite, name them, add the stock to the model and the dashboard. Don't tune yet.
Caveat: composite metrics can be gamed; pick indicators that resist trivial optimization (e.g., depth-of-feature-use beats login-count).
Pattern: meter-in-basement (the relationship state is happening but isn't visible to the actors making decisions about it)
```

```
LEVERAGE CANDIDATE #2
Meadows knob: 3 (Goals — name the stated/enacted divergence)
What to change: the implicit system goal. Structure currently enacts "maximize MRR-per-quarter via transactional acquisition." Stated goal is "deep relationships with first 50 design-partner customers." Pick one and re-wire.
Intuitive push: keep both — claim deep relationships AND chase MRR
Correct push: cap MRR growth or cap headcount of SDRs until CSM_count ≥ some ratio of signed_customers; OR explicitly demote the relationship goal to "secondary, after we hit 100 customers."
Why intuition reverses it: founders defend the dual narrative because both feel important; the structure keeps quietly winning anyway because R1 is the only closed loop touching MRR.
Why it has leverage: as long as the enacted goal is unchallenged, every other intervention will get absorbed into serving it.
First move (this month): write down which goal wins on conflict. Tell the team. Add the conflict-resolution rule to the sales / CS playbook.
Caveat: choosing "relationships first" likely slows MRR — be honest with whoever you report to before, not after.
```

### Pass criteria
- ✅ Both Phase 1.5 (structural MISMATCH) and Phase 1.6 (DIVERGENT) fire
- ✅ Candidate #1 = structural noun fix (Phase 1.5); Candidate #2 = semantic divergence fix (Phase 1.6)
- ✅ Pattern `meter-in-basement` correctly tagged on Candidate #1
- ✅ Direction fields populated on both candidates and they are opposite to intuition

---

## Test 6: Accountability gap (meter-in-basement)

### Input

**Goal:** "Cut weekly cloud bill by 30% by end of quarter without slowing feature velocity."

**Model:**
- Stocks: monthly_cloud_spend, feature_velocity, on_call_load
- Flows: spend_growth (driven by per-team resource requests), spend_reduction (driven by infra team optimization sprints)
- Parameters: per_team_budget_visibility (CURRENTLY: only the CTO sees per-team spend), num_teams (8), infra_team_size (3)
- Feedback loops: R1 — more spend → more on_call → more reactive provisioning → more spend (reinforcing). No loop closes from cost back to the engineering team making the deploy decisions.

### Expected skill output (key check)

**Phase 1 accountability-gap detection:** Decision = "provision N instances for new service." Actor = product engineering team. Consequence = cloud bill. Actor visibility into consequence = none (per `per_team_budget_visibility` parameter — only CTO sees the per-team breakdown).

```
LEVERAGE CANDIDATE #1
Meadows knob: 6 (Information flows — restore missing feedback)
What to change: per_team_budget_visibility parameter — flip from CTO-only to per-team-weekly. Add a single auto-posted Slack message every Monday: "Team X spent $Y last week, $Z week-over-week." (This adds an info-flow node — allowed structural addition.)
Intuitive push: do nothing on info; instead, hire more infra people or impose a top-down budget cap
Correct push: just put the meter in the front hall — let each team see its own spend, weekly, in a place they already look
Why intuition reverses it: the assumption is that engineers "don't care about cost" — actually they don't see it. Dutch electricity meter result: same actors, same system, 30% reduction just from visibility. The cheap intervention beats the expensive one.
Why it has leverage: closes a missing feedback loop without changing rules, hiring, or rebuilding infrastructure. Meadows' chapter calls this the most common and cheapest high-leverage class.
First move (knob ≤6 → this month): pick the 3 most expensive teams, send them last week's number on Monday, repeat for 4 weeks. Measure spend delta.
Caveat: visibility without context can blame innocent teams (research workloads cost more by design). Annotate the post: "Cost ≠ wastefulness — context: research vs prod."
Pattern: meter-in-basement (literal application of Meadows' Dutch housing example)
```

### Pass criteria
- ✅ Accountability gap detected in Phase 1 extraction
- ✅ Candidate #1 uses the allowed structural-addition exception for knob 6
- ✅ Pattern `meter-in-basement` explicitly named and cited as direct application of Meadows
- ✅ First move is a one-month rollout, not a 2-week experiment

---

## Test execution notes (for Bayram pre-workshop)

Run all three tests in:
1. Claude Code with `/leverage-finder`
2. ChatGPT (free tier) with PROMPT.md pasted
3. Claude.ai (free tier) with PROMPT.md pasted

Verify:
- Phase 0 gate triggers on Test 3 in all three runtimes
- Tests 1 and 2 produce 3 candidates with at least one at knob ≤5
- No runtime invents stocks/parameters not in the original model
- Output is readable on mobile (paste it into Telegram and look at it on phone)

**Time budget for this validation: 25 min total. If any runtime fails: drop PROMPT.md from the workshop and require Claude Code or Claude.ai (both passed in W3).**
