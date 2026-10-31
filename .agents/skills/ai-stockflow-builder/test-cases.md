# Test cases — `ai-stockflow-builder`

Three worked examples covering the participant archetypes from W3 prep. Each test specifies: input the participant brings, expected Phase A response (readback + clarifying questions), expected Phase B brief structure, and an extraction-test value the agent's app must match.

---

## Test 1 — Stanislav: support ticket backlog with hire-step

**Domain:** support ops at a SaaS startup. The participant has been told (W2) the dynamic is Limits to Growth: arrivals are constant, capacity is the bottleneck.

### Input the participant pastes after invoking the skill

```
Stocks:
- Backlog: 200 tickets

Flows:
- Arrivals: from null to Backlog, rate = ArrivalRate, units: tickets/week
- Resolutions: from Backlog to null, rate = min(Backlog, Agents * Throughput), units: tickets/week
- Hire: instantaneous step at week HireWeek, adds 1 agent

Parameters:
- ArrivalRate: default 50, min 20, max 100, step 5, units: tickets/week
- Agents: default 1 (becomes 2 at HireWeek), min 1, max 5, step 1
- Throughput: default 30, min 10, max 60, step 5, units: tickets/week/person
- HireWeek: default 4, min 1, max 12, step 1, units: week

Reference mode:
Backlog stayed flat at ~50 tickets for months, then surged to 200 when one agent quit. We hired a replacement in week 4. Backlog should peak then decline.
```

### Expected Phase A response (skill output)

The skill should:
- Read back: 1 stock (Backlog=200), 3 flows (Arrivals in, Resolutions out, Hire as instantaneous boost), 4 parameters
- Surface ≥2 clarifying questions, e.g.:
  - "Hire is described as 'instantaneous step at week HireWeek'. Is Agents a stock that goes from 1 to 2 at that week, or is it a parameter that flips? The dynamics differ subtly — if Agents is a stock, it's persistent; if it's a parameter expression `(t < HireWeek ? 1 : 2)`, it's a function of time."
  - "Resolutions = `min(Backlog, Agents * Throughput)` — this clamps when Backlog is small. Is that intentional (you don't want negative resolutions when backlog is empty), or should it be just `Agents * Throughput` and let the stock-clamp-at-zero handle it?"
- NOT emit the brief yet

### Expected Phase B brief (after participant clarifies: "Agents is a parameter expression; resolution clamp is intentional")

The skill emits a brief with:
- Title: "Support ticket backlog with hire-step"
- Time unit: weeks
- Time horizon: 52
- Stocks: `Backlog: 200`
- Flows: `Arrivals` rate `ArrivalRate`; `Resolutions` rate `min(Backlog, (t < HireWeek ? StartAgents : EndAgents) * Throughput)` (or equivalent — the skill normalizes the hire-step into an explicit conditional)
- Parameters: ArrivalRate, StartAgents (default 1), EndAgents (default 2), Throughput, HireWeek
- Extraction test comment: at t=0 with defaults, Arrivals=50, Resolutions=min(200, 1*30)=30, Backlog[1] = max(0, 200+50−30) = 220

### Pass criteria for the extraction test

Agent's `model.html` must show Backlog ≈ 220 at t=1 with default sliders. Tolerance ±0.1.

---

## Test 2 — Marfa: focus stock with regenerating depletion

**Domain:** founder energy management. From Marfa's W2 self-intro: she frames "available focus" as a stock that depletes through context-switching and regenerates on rest days.

### Input

```
Stocks:
- Focus: 100 units (full attention budget)

Flows:
- Depletion: from Focus to null, rate = Switches * SwitchCost, units: focus/day
- Regeneration: from null to Focus, rate = (RestDay ? RegenPerRestDay : 0), units: focus/day

Parameters:
- Switches: default 8, min 0, max 30, step 1, units: switches/day
- SwitchCost: default 5, min 1, max 15, step 1, units: focus/switch
- RegenPerRestDay: default 80, min 20, max 100, step 5, units: focus/day
- RestDay: default false (boolean), the flag is true on weekends

Reference mode:
Focus drops sharply Mon-Fri (workdays with switches), partially recovers Sat-Sun. Over a month it trends down because regen doesn't fully cover depletion.
```

### Expected Phase A response

Surface:
- "RestDay is described as 'true on weekends' — how does the simulation know which days are weekends? Is it `t mod 7 in [5, 6]` (weekly cycle), or do you want a slider for 'how many rest days per week'?"
- "Focus has a hard ceiling at 100 (your initial value, full budget) — should the simulation clamp UP at 100 as well as down at 0? Right now the spec only mentions clamp-at-zero."
- "Switches and SwitchCost are constant across all days, but you said 'workdays have switches' — do switches go to 0 on rest days? Or is the cost just lower?"

### Expected Phase B brief (after Marfa clarifies: weekly cycle via t%7, ceiling clamp at 100, switches=0 on rest days)

The brief includes:
- A `RestDay` derived expression: `(t % 7 >= 5)` instead of a slider
- Depletion equation: `(t % 7 < 5 ? Switches * SwitchCost : 0)`
- An explicit max-clamp note: "stocks must clamp at 100 maximum, not just 0 minimum, for this model"

This is a **deliberate hard case for the skill** — the canonical brief template clamps at 0 only. The skill must augment the iron-constraints section with an explicit max-clamp instruction when the user requests it.

### Pass criteria

At t=1 (a Tuesday, second day in cycle): Focus = max(0, min(100, 100 + 0 − 8*5)) = 60.
At t=5 (Saturday, first rest day): Focus depletes 4 weekdays × 40/day = 160 → clamps at 0 by mid-week, then on Saturday +80 → 80.

The skill's extraction test should give the participant *one specific value*: "at t=1 (Tuesday), Focus should be 60. Verify."

---

## Test 3 — Vlad: hiring pipeline with two stages

**Domain:** recruitment funnel for an engineering org. Two-stock model.

### Input

```
Stocks:
- Candidates: 0
- Hires: 0

Flows:
- Sourcing: from null to Candidates, rate = SourcingRate, units: candidates/week
- Conversion: from Candidates to Hires, rate = Candidates * OfferAcceptRate / TimeToHire, units: candidates/week
- Attrition: from Hires to null, rate = Hires * AttritionRate, units: people/week

Parameters:
- SourcingRate: default 10, min 0, max 50, step 1, units: candidates/week
- OfferAcceptRate: default 0.4, min 0.1, max 1.0, step 0.05, units: dimensionless
- TimeToHire: default 6, min 2, max 12, step 1, units: weeks
- AttritionRate: default 0.02, min 0, max 0.1, step 0.005, units: 1/week

Reference mode:
Started year with 0 hires. Goal: 50 hires by end of year. We've been sourcing 10/week, converting at 40%, but TimeToHire is creeping up.
```

### Expected Phase A response

Surface:
- "Conversion = `Candidates * OfferAcceptRate / TimeToHire` is unusual — typically TimeToHire is a delay, not a divisor in the rate. Are you sure the rate is `Candidates / TimeToHire * OfferAcceptRate` (i.e., `1/TimeToHire` of the candidate pool flows out per week, scaled by accept rate), or did you mean something else?"
- "Attrition is on Hires (the cumulative count). Is that right — current employees attriting? Or did you mean attrition during the candidate stage?"
- "Reference mode: 50 hires in 52 weeks = ~1/week. Sourcing 10 × 0.4 / 6 ≈ 0.67/week. Already short of target — is the simulation supposed to expose that, or should we calibrate the params to hit 50?"

### Expected Phase B brief

A two-stock brief with:
- Stocks: Candidates=0, Hires=0
- Flows: Sourcing in to Candidates, Conversion from Candidates to Hires, Attrition out of Hires
- Extraction test: at t=0, Candidates=0, Hires=0. Sourcing=10. Conversion=0*0.4/6=0. Attrition=0*0.02=0. At t=1: Candidates=10, Hires=0.

### Pass criteria

App must show two lines (Candidates primary, Hires secondary). Candidates ramps from 0 to ~30-40 then plateaus; Hires grows steadily.

---

---

## Test 4 — Andrey: interview mode (no diagram)

**Domain:** founder of a small accounting firm. Comes to W3 without a hand-drawn diagram — only a problem statement.

### Trigger

Participant invokes the skill and pastes:

```
I don't have a diagram. My problem: my consulting firm's revenue has been flat for 6 months despite hiring 2 more consultants. I want to understand why, then simulate it. Interview me.
```

### Expected skill behavior — Phase 0 sequence

The skill should NOT propose any stocks, flows, or parameters. It should ask one Socratic question at a time. Expected sequence:

**Q1 (problem & reference mode):**
> "What's the variable you actually care about — the one that's behaving in a way you want to change? Tell me how it has behaved (rough numbers + dates), and where you want it to be."

User responds: "Monthly revenue. Was 100k Jan, 110k Feb-Mar, then flat at 110k Apr-Sep despite hiring. Goal: 200k by year-end."

**Skill must push back (Socratic):**
> "Monthly revenue is a *rate* (dollars per month), not a stock. But the thing you care about — is it the *flow* (each month's earnings) or the *level* it would accumulate to (cumulative revenue, ARR equivalent)? If the world stopped at midnight tonight, would you snapshot 'this month's revenue so far' or 'cumulative revenue this year'?"

User responds: "I think I care about active clients. Each client pays a retainer, so monthly revenue is clients × retainer. Active clients dropped from 50 to 45 even though I hired more consultants."

**Q2 (stocks):**
> "Good — so 'Active clients' is a stock you snapshot at midnight. Are there other things that accumulate in this problem? Apply the midnight test: what could you count if the world froze right now? Limit to 1-3."

User responds: "Active clients. And maybe consultants — I went from 4 to 6."

**Skill output:** "Two stocks confirmed: Active Clients (current 45), Consultants (current 6). Initial values for the simulation?"

User: "Start simulation at Jan = 50 clients, 4 consultants."

**Q3 (flows):**
> "For Active Clients: what makes it go UP? What makes it go DOWN? For Consultants: same. Name each flow + describe the rate in words. Probe: does the inflow depend on the stock itself? Are there delays?"

User responds: "Up: new clients from referrals. Probably proportional to current clients × referral rate. Down: churn. Maybe higher when consultants are overloaded? Up consultants: hiring decision (I control it). Down consultants: attrition, ~5%/year."

**Skill must push:**
> "'Higher churn when consultants overloaded' — what's the load measurement? Is it Clients/Consultants? And is the relationship linear (1% churn per unit overload) or threshold-based (no churn until load >12, then jumps)?"

User: "Linear. Each Client/Consultant ratio above 10 adds 1%/month to churn."

**Q4 (parameters):**
> "What are the dials you can change? For each: default, min, max, units. Limit to 3-6."

User: "ReferralRate: default 0.05/month, range 0.02-0.10. BaseChurnRate: 0.03/month, range 0.01-0.08. LoadThreshold: 10 clients/consultant, range 6-15. HiringRate: 0/month default (means no hiring), range 0-2/month. AttritionRate: 0.005/month."

**Q5 (negative-stock):**
> "If Active Clients would go below zero, what should physically happen? Stay at zero or allow negative?"

User: "Stay at zero."

**Q6 (recap):**

Skill outputs in canonical format:
```
Stocks:
- ActiveClients: 50
- Consultants: 4

Flows:
- NewClients: from null to ActiveClients, rate = ActiveClients * ReferralRate, units: clients/month
- Churn: from ActiveClients to null, rate = ActiveClients * (BaseChurnRate + max(0, ActiveClients/Consultants - LoadThreshold) * 0.01), units: clients/month
- Hiring: from null to Consultants, rate = HiringRate, units: consultants/month
- Attrition: from Consultants to null, rate = Consultants * AttritionRate, units: consultants/month

Parameters:
- ReferralRate: 0.05, [0.02, 0.10], step 0.005, units: 1/month
- BaseChurnRate: 0.03, [0.01, 0.08], step 0.005, units: 1/month
- LoadThreshold: 10, [6, 15], step 1, units: clients/consultant
- HiringRate: 0, [0, 2], step 0.25, units: consultants/month
- AttritionRate: 0.005, [0, 0.02], step 0.001, units: 1/month

Reference mode: Started Jan with 50 active clients, 4 consultants. Hired 2 more consultants over 6 months but Active Clients dropped to 45. Goal: understand why 200k revenue (≈90 clients × ~2.2k retainer) didn't happen.
```

> "Confirm or correct, then we'll move to Phase A — the readback."

### Pass criteria for Test 4

- Skill **never** proposed an entity. Every named stock, flow, and parameter traces to user's words.
- Skill **redirected** when user named a rate as a stock (monthly revenue → active clients).
- Skill **probed** linearity vs. threshold on the load-churn relationship.
- Skill produced a structured model in the canonical format.
- After Phase 0, user proceeds to Phase A and gets a normal readback.

### Expected extraction-test value

After Phase 0 → Phase A → Phase B → agent builds app:
At t=0: ActiveClients=50, Consultants=4. NewClients=50*0.05=2.5. Load = 50/4 = 12.5. ChurnFactor = 0.03 + max(0, 12.5-10) * 0.01 = 0.03 + 0.025 = 0.055. Churn = 50 * 0.055 = 2.75. ActiveClients[1] = max(0, 50 + 2.5 − 2.75) = **49.75**. (Decreasing — overload churn dominates new acquisition.)

This is **the diagnostic moment**: at workshop, the simulation surfaces that the firm's growth bottleneck isn't capacity (consultants), it's the load-driven churn loop. Andrey adds a consultant, load drops, churn drops, growth resumes. Sterman's "structure drives behavior" lesson lands.

---

---

## Test 5 — Stanislav under Pass 1 caps (trim conversation)

**Setup:** Stanislav brings the same model as Test 1 (Backlog stock, Arrivals/Resolutions/Hire flows, 4 parameters, conditional resolution equation with `min()` and ternary). At workshop time 0:40, he enters the skill and selects **Pass 1**.

### Expected skill behavior

The skill must NOT proceed to readback. It must run the trim conversation:

**Skill (after entry question = Pass 1):**

> Pass 1 caps: 1 stock, 2 flows, 2 parameters, linear equations only, ≤30 steps. Your model has 1 stock ✓, 3 flows ✗ (Arrivals, Resolutions, Hire), 4 parameters ✗, and the Resolutions equation is non-linear (`min(Backlog, ...)` + ternary). Let's trim before we build.
>
> 1. **Flows.** Hire is essentially a step-change on Agents. For Pass 1, drop Hire entirely — make Agents a constant. Keep Arrivals (in) and Resolutions (out). OK?
> 2. **Parameters.** Of {ArrivalRate, Throughput, StartAgents, EndAgents, HireWeek}, pick the 2 dials you most want to scrub. The other 3 become constants. My recommendation: keep ArrivalRate (uncertain demand) and Throughput (operational improvement); fix Agents=2 (the steady-state count after the hire), drop StartAgents/EndAgents/HireWeek for Pass 1. Agree?
> 3. **Equation.** With those choices, Resolutions = `Agents * Throughput` (constant rate, no min). Backlog clamps at zero — the canonical brief handles that. Linear. ✓
>
> Confirm these trims and I'll write the Pass 1 readback.

### Pass criteria for Test 5

- Skill must REFUSE to start readback before trim is complete
- Skill must propose specific concrete trims (not just "you have too many things")
- After trim: 1 stock (Backlog=200), 2 flows (Arrivals in: rate ArrivalRate; Resolutions out: rate Agents * Throughput), 2 parameters (ArrivalRate, Throughput), constants (Agents=2), linear equations, time horizon=30
- Extraction test at t=1 with defaults (ArrivalRate=50, Throughput=30, Agents=2): Arrivals=50, Resolutions=2*30=60, Backlog[1] = max(0, 200+50−60) = 190
- Same skill in Pass 2 mode the second time he runs it must REJOIN the trimmed parts: Stanislav adds Hire, HireWeek, conditional resolution. Pass 2 readback proceeds without trim refusal.

### What this test probes

| Aspect | Probe |
|--------|-------|
| Cap enforcement | Skill refuses readback when caps violated |
| Trim conversation quality | Concrete recommendations, not "you decide" |
| Linear-only constraint | `min()` + ternary explicitly removed |
| Pass 1 → Pass 2 transition | Same model gets full treatment in Pass 2 |

---

## What this test suite probes

| Test | Probes |
|------|--------|
| 1 (Stanislav) | Hire-step encoding (param vs. stock); skill's normalization of "instantaneous step at week N" into an explicit conditional |
| 2 (Marfa) | Skill's handling of edge cases the canonical brief doesn't cover (max-clamp); periodic time function (t%7); boolean parameter |
| 3 (Vlad) | Two-stock model; flow connecting two stocks (not just from/to null); skill's ability to spot equation suspicion (TimeToHire as divisor); calibration question |
| 4 (Andrey) | **Interview mode (Phase 0)**: skill must elicit a model without proposing entities; redirect rate-as-stock confusion; probe linear-vs-threshold relationships; recap in canonical format |
| 5 (Stanislav Pass 1) | **Simplified mode**: hard-cap enforcement, trim conversation quality, linear-only constraint |

Pass = skill correctly identifies the issue in each test's "Expected Phase A" before emitting the brief, and the brief's extraction-test value matches by-hand math.

## How to run a test

1. Open Claude Code in a clean directory.
2. Activate the skill: `/ai-stockflow-builder`
3. Paste the test's input.
4. For tests 1-3: verify Phase A surfaces the expected questions. For test 4: verify Phase 0 elicits the model without proposing entities.
5. Answer the clarifying questions as the test specifies.
6. Verify Phase B brief includes the expected structure.
7. Paste the brief into a *fresh* Claude Code session in a new directory.
8. Let the agent build `model.html`.
9. Open `model.html` in browser, verify the extraction-test value at t=1.
10. PASS if value matches; FAIL otherwise. If FAIL, inspect the agent's code for which equation diverged.
