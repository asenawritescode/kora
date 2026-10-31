# Workflow blueprint - the engine

This is the copy-paste `Workflow` script that powers Phase 2. It fans out lenses, dedups, deepens + grades, **audits the load-bearing measurements** (the raw→validated ladder), refutes, and converges. **Scale it to the chosen depth** by adjusting `LENSES`, `LOOP_UNTIL_DRY`, and the refute vote count (see the DEPTH block).

The script is plain JavaScript (no TypeScript). `agent()`, `parallel()`, `pipeline()`, `phase()`, `log()` are the workflow hooks. Pass research-source paths/URLs into the agent prompts via a constant you fill in from Phase 1. Agents reach web tools (Exa/firecrawl) and Read/Grep via ToolSearch on their own.

## The tree-JSON schema (the contract for the visualization)

The workflow must end by returning an object matching this shape. `assets/tree-template.html` renders exactly this.

```json
{
  "apex": "Net-new signups are short of the N target by <date>",
  "verdict": "One-line answer / headline (the so-what) - rendered prominently under the apex. apex is the STATEMENT of the undesirable effect; verdict is the ANSWER.",
  "stages": [
    {
      "id": "S1", "title": "Stage 1 - Arrive",
      "nodes": [
        {
          "id": "A", "text": "Discovery gap - net-new users never find the surface",
          "kind": "INFERENCE", "grade": "Strong", "role": "LEVER",
          "cite": "web: 3 competitor listings rank; ours absent (SERP Jun)",
          "children": [
            { "id": "A1", "text": "...", "kind": "MEASURED", "grade": "Mod", "validation": "validated", "cite": "src:warehouse — ..." }
          ]
        }
      ]
    }
  ],
  "roots": [
    { "id": "R0", "title": "No one owns activation because it falls between PM and Growth (POLICY, not tooling)",
      "from": ["M","Q"], "action": "Name a DRI; break the evaporating cloud", "isConstraint": true,
      "children": [
        { "id": "R0a", "text": "Activation has no DRI because it spans two orgs", "kind": "INFERENCE", "grade": "Mod", "cite": "...",
          "children": [
            { "id": "R0b", "text": "Each org optimizes its own metric, so the cross-org gap is nobody's number", "kind": "CLAIM", "grade": "Strong", "cite": "...",
              "children": [ { "id": "R0c", "text": "BEDROCK: the comp plan rewards per-org KPIs, not the shared funnel (deliberate policy)", "kind": "EXTERNAL", "grade": "Strong", "cite": "..." } ] }
          ] }
      ] },
    { "id": "R1", "title": "...", "from": ["A1","L"], "action": "...", "isConstraint": false, "children": [] }
  ],
  "constraint": { "rootId": "R0", "type": "policy",
    "statement": "System constraint = ownership/policy; located, needs no more data.",
    "unlocated": "Mechanism constraint still unlocated - one query pins it." },
  "negatives": [
    { "text": "Auto-invite a user's whole contact list -> spam complaints + a deliverability cliff", "restsOn": [] },
    { "text": "Keystone on a behavior the data says doesn't exist (0 of 80 trials did it)", "restsOn": ["A1"] }
  ],
  "tests": [
    { "test": "Join origin -> completion + signup-date (1 query)",
      "decides": "Adjudicates C, D, G at once; arrival-starved vs conversion-problem" }
  ],
  "dataRequest": [
    { "want": "org_created + first_invite events (not currently logged)", "upgrades": ["N"],
      "from": "instrument in product analytics", "kind": "external", "rank": "#1",
      "impact": "Locates the funnel constraint — flips the mechanism from map to verdict" },
    { "want": "Whether a DRI already owns activation today", "upgrades": ["R0a"],
      "from": "ask the Growth + Product leads (in-head, not in any system)", "kind": "ask-owner", "rank": "#2",
      "impact": "If someone owns it, the policy constraint re-roots — confirm before acting" }
  ],
  "census": { "measured": 0.45, "inference": 0.25, "hypothesis": 0.20, "claim": 0.07, "external": 0.03,
    "bottomLine": "Broad but shallow at the decision point - the constraint-locating nodes are HYPOTHESIS. Map, not verdict." },
  "apexType": "symptom",
  "verdictStatus": "map",
  "narrative": [
    { "heading": "The problem", "body": "One-line restatement of the apex a reader sees before the tree." },
    { "heading": "What the evidence forces", "body": "The verdict/reframe in plain prose — what the graded data makes us conclude." },
    { "heading": "The constraint, and the honest risk", "body": "The one binding cause, plus what flips if its weakest support is wrong." }
  ]
}
```

`kind` ∈ MEASURED|INSTANCE|EXTERNAL|CLAIM|INFERENCE|HYPOTHESIS|FRAMING. `grade` ∈ Strong|Mod|Weak. `role` ∈ ROOT|CONSTRAINT|LEVER|NEGATIVE|REFUTED|DOWNGRADED|"" (see `answer-kinds.md`). `validation` (MEASURED nodes only) ∈ raw|validated|triangulated|contested — the raw→validated ladder from `answer-kinds.md`; `raw` caps the grade at Mod and can never be `constraint.loadBearing`. `verdictStatus` ∈ verdict|map — **computed by the orchestrator after converge, never emitted by an agent** (see step 7d): `map` unless the loadBearing node is solid. The `census` counts are likewise **orchestrator arithmetic over the final tree** (step 7d²) — the converge agent supplies only `bottomLine`.

**Two apex types — and two output shapes.** `apexType` ∈ `symptom` | `target`. A **symptom** apex ("X is leaking / under-converting / broken") wants the classic CRT: converge to the ONE binding constraint. A **target** apex ("hit N by date / grow X→Y" — a number someone *chose*, reachable by several ADDITIVE paths) wants that *plus* a **lever portfolio**: the parallel paths to N, each sized, so the output is "here's the bottleneck AND here's the arithmetic for whether the levers sum to N" — not a lone bottleneck. In target mode the converge step still finds the constraint, and an extra **lever-decomposition** stage emits `levers[]` + `sizing` (below). **`narrative[]`** (both modes) is a short memo rendered at the TOP of the HTML so the artifact self-explains before anyone drills the tree.

```json
{ "...": "target-apex additions (only when apexType=='target')",
  "levers": [
    { "id":"L1", "name":"Acquire net-new via standalone tools", "path":"proposalpanda → free signup → programmatic send",
      "sizing": { "volume":"~X relevant signups/mo", "conversion":"~Y% → MSA", "contribution":"≈ Z toward N", "basis":"HYPOTHESIS — funnel not yet instrumented (gap:experiment)" },
      "ownWinBuy":"win", "effort":"high", "bindsConstraint":true, "role":"primary" },
    { "id":"L2", "name":"Migrate the existing base onto the agent", "path":"in-product nudge → base adopts connector",
      "sizing": { "volume":"large installed base", "conversion":"low — base sends in UI by choice", "contribution":"≈ small", "basis":"MEASURED: 71% send via UI" },
      "ownWinBuy":"own", "effort":"med", "role":"dropped", "droppedReason":"Off-strategy vs the net-new target + base won't switch (71% UI by choice) — kept visible, not silently cut." }
  ],
  "sizing": { "target":"5,000 by EOY", "byDate":"Dec", "sumOfLevers":"≈ L1 only, unproven", "gapToTarget":"the whole number rests on the one unproven lever",
    "bottomLine":"The levers do NOT credibly sum to N yet — the target leans entirely on the un-instrumented L1." }
}
```

**Idea Audit additions (only when the run evaluates an idea).** The idea is captured at Phase 0 and **quarantined**: passed in via `args.idea` but NEVER interpolated into a scout/lens/deepen/refute prompt, and scrubbed from the frozen evidence brief (anti-anchoring — a swarm that knows the pitch bends toward the causes the pitch needs). Only the post-converge fit-check stage (7b², below) reads it, emitting `idea` + `ideaFit`:

```json
{ "...": "idea-audit additions (only when args.idea is set)",
  "idea": { "statement":"Ingest meeting recordings + docs → auto-generate an onboarding program + knowledge search",
    "claimedProblem":"Contract engineers take 3-5 months to onboard, with no early signal on fit or progress",
    "mechanism":"knowledge navigation", "buyer":"the client? the staffing contractor? — unowned" },
  "ideaFit": {
    "verdict": "misses-constraint",
    "addresses": ["A1","C2"],
    "why": "The idea shortens knowledge navigation; the located constraint is ownership of the onboarding process — a policy node the tool leaves untouched.",
    "restsOn": ["M","R0a"],
    "policyToolMismatch": { "flag": true, "note": "Constraint is policy/ownership; a tool that doesn't move ownership can't dissolve it." },
    "absorbedPain": { "state":"chronic-absorbed", "kind":"HYPOTHESIS", "grade":"Mod", "gap":"experiment",
      "cite":"15-20-year-old systems have budgeted around slow onboarding for as long — the cost is internalized",
      "test":"Pre-sell to 5 target buyers; check whether any budget line or adjacent purchase exists" },
    "segmentFork": { "diesWhere":"client-owned onboarding of 1-2 contractors, pain absorbed for years",
      "livesWhere":"100-person cohorts with an acute trigger and a named onboarding owner",
      "kind":"HYPOTHESIS", "gap":"external", "test":"5 discovery calls in the lives-where segment" },
    "redirect": { "reframedProblem":"Know within 2 weeks whether engineer A is onboarding better than B",
      "whatWouldDissolve":"An early-signal instrument owned by the contractor — plus the ownership move itself" },
    "refutation": { "contested":"the fit skeptic's counter-argument (or {survived: what it attacked and lost on} / {skipped: 'fit skeptic failed to run'})" },
    "contested": "mirror of refutation.contested — kept for the renderer; absent when the fit survived"
  }
}
```

`ideaFit.verdict` ∈ `dissolves-constraint` | `relieves-symptom` | `misses-constraint` | `negative-branch`. Every check is an evidence-graded claim (kind/grade + a nameable test), never a pronouncement; `addresses`/`restsOn` must reference real node ids so the update loop's orphan sweep covers the idea verdict like any recommendation.

## Converge schema (paste into the converge `agent()` call) — prevents the two bugs seen in testing

Use this STRICT JSON Schema for the converge step. A **loose schema lets the agent invent its own structure that won't render** (observed in the first smoke run), and without a length/semantic constraint the agent **overstuffs `apex` with the whole verdict** (observed in a real run, broke the header). The schema + prompt below fix both: `apex` is a SHORT undesirable-effect STATEMENT (≤140 chars, NOT a question), the one-line answer goes in `verdict`, and `constraint.statement` stays ≤ ~280 chars.

```javascript
const NODE = { type:'object', properties:{
  id:{type:'string'}, text:{type:'string'},
  kind:{type:'string', enum:['MEASURED','INSTANCE','EXTERNAL','CLAIM','INFERENCE','HYPOTHESIS','FRAMING']},
  grade:{type:'string', enum:['Strong','Mod','Weak']},
  role:{type:'string', enum:['ROOT','CONSTRAINT','CONSTRAINT?','LEVER','NEGATIVE','REFUTED','DOWNGRADED','critic-added','']},
  // gap (only on HYPOTHESIS/CLAIM nodes): 'in-hand' = one read over Phase-1 sources settles it (ILLEGAL to leave —
  // compute + re-grade MEASURED) · 'external' = needs data we don't have · 'experiment' = needs a test.
  gap:{type:'string', enum:['in-hand','external','experiment']},
  // validation (MEASURED nodes only): 'raw' = pipeline unchecked (grade caps at Mod; cannot be loadBearing) ·
  // 'validated' = passed the cleanliness checklist / curated source · 'triangulated' = two independent pipelines
  // agree · 'contested' = two sources disagree (grade Weak; keep BOTH values in cite; reconcile via tests[]).
  validation:{type:'string', enum:['raw','validated','triangulated','contested']},
  cite:{type:'string'}, children:{type:'array'} }, required:['text','kind','grade'] }
const TREE_JSON_SCHEMA = { type:'object', properties:{
  apex:{type:'string'},          // SHORT undesirable-effect STATEMENT (<=140 chars; declarative, NOT a question, NOT the answer)
  verdict:{type:'string'},       // the one-line answer / headline
  stages:{ type:'array', items:{ type:'object', properties:{
    id:{type:'string'}, title:{type:'string'}, crosscut:{type:'boolean'}, nodes:{type:'array', items:NODE} },
    required:['title','nodes'] }},
  roots:{ type:'array', items:{ type:'object', properties:{
    id:{type:'string'}, title:{type:'string'}, from:{type:'array', items:{type:'string'}},
    action:{type:'string'}, isConstraint:{type:'boolean'},
    // restsOn = the node ids the ACTION stands on — the update loop's orphan sweep retracts an action
    // whose restsOn node dies (a field tree recommended a build whose supporting node one test killed).
    restsOn:{type:'array', items:{type:'string'}},
    // children = the nested causal chain UNDER this root (root -> sub-cause -> ... -> bedrock).
    // REQUIRED and non-empty for the constraint root; other roots may keep it shallow/empty.
    children:{type:'array', items:NODE} }, required:['id','title'] }},
  constraint:{ type:'object', properties:{
    rootId:{type:'string'}, type:{type:'string'}, statement:{type:'string'}, unlocated:{type:'string'},
    loadBearing:{type:'string'},  // id of the ONE node the constraint most rests on (its weakest-grade support) — drives the viz stress-test
    ifFalse:{type:'string'} },    // what changes if loadBearing is wrong: which root takes over / verdict reverts to map
    required:['rootId','statement'] },
  // negatives as {text, restsOn:[nodeIds]} — restsOn makes the anti-recommendation traceable to its
  // evidence exactly like root actions. (The viz also renders legacy plain strings; emit objects.)
  negatives:{ type:'array', items:{ type:'object', properties:{
    text:{type:'string'}, restsOn:{type:'array', items:{type:'string'}} }, required:['text'] }},
  // corrections: 1-3 one-liners for claims the refute pass KILLED or DEMOTED, each with its killing
  // evidence — the viz renders these as the red "read first — live corrections" banner.
  corrections:{ type:'array', items:{type:'string'} },
  tests:{ type:'array', items:{ type:'object', properties:{
    test:{type:'string'}, decides:{type:'string'}, rank:{type:'string'} }, required:['test','decides'] }},
  // dataRequest = the "what I need from you to turn this map into a verdict" shopping list.
  // Built ONLY from external/experiment gaps (in-hand gaps must already be computed). Each item ties to the
  // real node id(s) it would upgrade HYPOTHESIS->MEASURED. May overlap tests[], but tests[] are fork-deciding
  // experiments while dataRequest[] is the missing-evidence ask, ranked by verdict-impact / cost.
  dataRequest:{ type:'array', items:{ type:'object', properties:{
    want:{type:'string'}, upgrades:{type:'array', items:{type:'string'}}, from:{type:'string'},
    kind:{type:'string', enum:['external','experiment','ask-owner']}, rank:{type:'string'}, impact:{type:'string'} },
    required:['want','impact'] }},
  census:{ type:'object', properties:{
    measured:{type:'number'}, inference:{type:'number'}, hypothesis:{type:'number'},
    instance:{type:'number'}, claim:{type:'number'}, external:{type:'number'}, bottomLine:{type:'string'} },
    required:['bottomLine'] },
  // provenance is STAMPED BY THE ORCHESTRATOR post-run ({method, depth, agents, date, triage}) — never
  // emitted by converge (it copied stray numbers when allowed to). Stamp `date` AFTER the workflow
  // returns: Date.now()/new Date() are unavailable inside workflow scripts.
  provenance:{ type:'object' } },
  required:['apex','verdict','stages','roots','constraint','census'] }
```

In the converge prompt, say explicitly: "**apex** = the undesirable-effect STATEMENT (≤140 chars, a declarative 'the bad thing', NOT a question); put the one-line answer in **verdict**; **PRESERVE THE CAUSAL CHAIN — do NOT flatten it.** 'Collapse to 3-5 roots' means *group* the branches, NOT compress a chain into a one-line root. The constraint root (`isConstraint:true`) MUST carry a nested `children[]` why-chain from the apex down to bedrock (root → because X → because Y → … → a bedrock cause: a market fact / deliberate policy / law of the domain / something outside our control), every node graded. A childless constraint root is a generation bug — the tree must read as a multi-level *tree*, not a 3-band funnel; phrase every `root[].title` and node `text` as a **declarative problem/cause statement** readable as 'X because Y' (never a topic/phase label like 'Cost denominator' or 'DM stage'); set exactly ONE root `isConstraint:true` and make `constraint.rootId` match it; keep `constraint.statement` tight. **Stages are the problem's journey/funnel phases ONLY — do NOT create a stage for apex / verdict / framing / scope content** (that belongs in `apex` + `verdict`, never a stage bucket), and use `role` values only from the enum (no invented roles like 'apex-answer'). Tag any test/recommendation that rests on an un-refuted CLAIM/HYPOTHESIS node with '(verify first)'. Set `constraint.loadBearing` = the id of the single node the constraint most rests on (its weakest-grade support — usually what the #1 cheapest test targets), and `constraint.ifFalse` = what changes if that node is wrong (which root takes over / the verdict reverts to a map). These drive the viz's stress-test. **Every root's `action` carries `restsOn: [real node ids]` (the evidence it stands on), and each negative may be `{text, restsOn}` — a recommendation that can't name its supporting nodes can't be retracted when they fall.** **PRESERVE every node's `validation` field exactly as the measurement audit set it — never upgrade `raw`→`validated` yourself. A node with `validation:'raw'` or `'contested'` may NOT be `constraint.loadBearing`; a `contested` node's reconciliation (which of the two pipelines is right?) must appear in `tests[]`/`dataRequest[]`.** **Tag every HYPOTHESIS/CLAIM node with a `gap` ∈ in-hand|external|experiment. An `in-hand` gap is ILLEGAL at converge — it means a read over the Phase-1 sources settles it, so compute it and grade MEASURED instead of emitting it. Then build `dataRequest[]` from the `external`/`experiment` gaps ONLY: each `{want, upgrades:[real node ids it lifts], from, kind (external|experiment|ask-owner), rank, impact}`, ranked by verdict-impact ÷ cost, with #1 = whatever would locate the constraint. `dataRequest` is the 'what I need from you' shopping list (the ROI-pattern: 'I lack the data, but here are the exact numbers that answer it'); every item must reference real node ids.**"

## The scout gate (Phase 1.5) — run this BEFORE the main workflow when no depth was picked

Clear-cut is measured, not read: three blind cheap passes either converge on an evidenced answer (ship it, skip the swarm) or demonstrate the confusability that justifies the spend. Run as its own small workflow so the routing decision (and the user's informed depth choice) happens between the two — interaction at the edges.

```javascript
export const meta = {
  name: 'why-tree-scout-gate',
  description: 'Triage: do 3 blind cheap passes converge on an evidenced root cause?',
  phases: [{ title: 'Scouts' }, { title: 'Judge' }, { title: 'Refute consensus' }],
}
// Parse args defensively (may arrive as a JSON string) + fail loud before spawning anything blind.
const A = typeof args === 'string' ? JSON.parse(args) : (args || {})
if (!A.apex) throw new Error('PRECONDITION FAILED: args.apex missing/unparsed')
const SCOUT_SCHEMA = { type:'object', properties:{
  constraint:{type:'string'},                       // ONE sentence: THE binding root cause
  decisiveEvidence:{ type:'object', properties:{ claim:{type:'string'}, source:{type:'string'},
    kind:{type:'string', enum:['MEASURED','INSTANCE','EXTERNAL','CLAIM','INFERENCE','HYPOTHESIS']},
    // validation is REQUIRED when kind==='MEASURED' — ship-direct demands validated/triangulated;
    // a raw number must never skip the swarm (that's the dirty-pipeline hole the skill exists to close)
    validation:{type:'string', enum:['raw','validated','triangulated','contested']} },
    required:['claim','source','kind'] },
  wouldRefuteMe:{type:'string'} }, required:['constraint','decisiveEvidence','wouldRefuteMe'] }

phase('Scouts')
const scouts = (await parallel([1,2,3].map(i => () =>
  agent(`You are ONE careful analyst (scout ${i} of 3, blind to the others). Diagnose: "${A.apex}"\n` +
        `Sources: ${A.sources}\nName THE single binding root cause (one sentence), the ONE decisive piece of ` +
        `evidence behind it (claim + source + answer-kind — HYPOTHESIS if you could not measure it; if MEASURED, also its ` +
        `validation: 'validated' ONLY if you checked the pipeline — denominator, metric definition vs claim, window — or the ` +
        `source is registry-curated; otherwise 'raw'), and the test that would prove you wrong. Commit to one answer; do not hedge across candidates.`,
        { label:`scout:${i}`, phase:'Scouts', schema:SCOUT_SCHEMA, effort:'low' })))).filter(Boolean)

phase('Judge')
const judge = await agent(
  `Three blind scouts diagnosed the same problem. Do they name the SAME binding constraint (semantically, not verbatim)?\n` +
  `${JSON.stringify(scouts)}\nReturn {converged:boolean, consensus:string|null, clusters:string}.`,
  { label:'judge', phase:'Judge', schema:{ type:'object', properties:{ converged:{type:'boolean'},
    consensus:{type:'string'}, clusters:{type:'string'} }, required:['converged','clusters'] } })

let refute = null
if (judge.converged) {
  phase('Refute consensus')
  refute = await agent(
    `Try to REFUTE this consensus diagnosis from the dirty-pipeline AND data-says-otherwise angles; default to refuted=true if the evidence is thin, unmeasured, or its pipeline is unchecked.\n` +
    `Consensus: ${judge.consensus}\nScouts' evidence: ${JSON.stringify(scouts.map(s=>s.decisiveEvidence))}\nSources: ${A.sources}`,
    { label:'refute:consensus', phase:'Refute consensus', schema:{ type:'object', properties:{
      refuted:{type:'boolean'}, evidence:{type:'string'} }, required:['refuted','evidence'] } })
}
// ship-direct requires MEASURED **validated/triangulated** decisive evidence from EVERY scout — a raw or
// contested number must never skip the swarm (Guardrail 11: consensus is not clear-cut).
const evidenced = judge.converged && scouts.every(s => s.decisiveEvidence.kind === 'MEASURED'
  && ['validated','triangulated'].includes(s.decisiveEvidence.validation))
return { scouts, judge, refute,
  decision: (judge.converged && evidenced && refute && !refute.refuted) ? 'ship-direct'
          : 'escalate',   // orchestrator routes: escalate → full run (or test-plan if decisive sources are human-only)
  receipt: judge.converged ? (refute && refute.refuted ? 'consensus KILLED by refuter: '+refute.evidence
                                                       : (evidenced ? 'consensus survived refutation' : 'consensus rests on unmeasured or unvalidated (raw/contested) support'))
                           : 'scouts diverged: '+judge.clusters }
```

Routing (orchestrator, after this returns): `ship-direct` → write the mini decision-doc from the consensus + receipt, stamp `provenance.triage`, stop. **Idea Audit × ship-direct:** there is no tree, so run the 7b² fit pair against the consensus `{constraint, decisiveEvidence}` with a DEGRADED contract — `addresses`/`restsOn` use the sentinel `['scout-consensus']` instead of node ids, and the mini-doc's idea section is FORCED to the verify-first treatment ("judged against a scout consensus, not a drilled tree"); a full orphanable verdict needs the full run. `escalate` → show the user the depth gate WITH the receipt (that's what they're buying the swarm to resolve), seed `scouts[].constraint` lines into the main run's dedup pool as extra branches — **never into the lens prompts** (anti-anchoring). Stamp the gate outcome into the final tree's `provenance.triage = {scouts:3, converged, decision, receipt}` so gate calibration is auditable across runs.

## The script

```javascript
export const meta = {
  name: 'why-tree',
  description: 'Build an evidence-graded Goldratt Why Tree for a problem',
  phases: [
    { title: 'Branch fan-out' }, { title: 'Deepen + grade' }, { title: 'Measure audit' },
    { title: 'Refute' }, { title: 'Converge' }, { title: 'Drill constraint' },
    { title: 'Levers + size' }, { title: 'Idea fit' }, { title: 'Narrate' },
  ],
}

// ---- DEPTH KNOBS (set from Phase 0 choice) ----
// HARDENING (field-proven 2026-07-04): `args` may arrive as a JSON *string*, not an object — parse
// defensively, then FAIL LOUD if the apex didn't arrive. A real run where args went unparsed sent
// 115 agents in with `undefined` in every prompt slot: no evidence brief, no scope guard — they
// freelanced onto live DBs and broke a blindness protocol. One assert vs a whole burned run.
const A = typeof args === 'string' ? JSON.parse(args) : (args || {})
if (!A.apex) throw new Error('PRECONDITION FAILED: args.apex missing/unparsed — refusing to launch a blind swarm')
const APEX = A.apex
const SOURCES = A.sources || 'web only (Exa/firecrawl)'      // Phase-1 inventory incl. the SOURCE REGISTRY ({id, tier, caveats} per source), injected into prompts
const DEPTH = A.depth || 'standard'                          // 'standard' | 'deep' | 'test-plan'  (Quick REMOVED — for a cheap sketch, ask ONE agent, not this skill)
const TEST_PLAN = DEPTH === 'test-plan'                      // decisive sources are HUMAN-ONLY → deliverable = branch-map + ranked cheapest-tests + Data Request (markdown); skips drill + the HTML machinery
const APEX_TYPE = A.apexType || 'symptom'                    // 'symptom' (X is leaking/breaking) | 'target' (hit N by date — a chosen number reached by additive paths)
const LEVER_MODE = APEX_TYPE === 'target'                   // target apex → ALSO decompose into additive levers + size them (a constraint tree alone hands ONE bottleneck, not a portfolio that sums to N)
// IDEA AUDIT (quarantined): {statement, claimedProblem, mechanism, buyer} — read ONLY by the fit-check
// stage (7b²). NEVER interpolate IDEA into a scout/lens/deepen/refute prompt, and make sure SOURCES /
// the frozen evidence brief was scrubbed of the pitch at Phase 1: a swarm that knows the pitch bends
// toward the causes the pitch needs, and the fit-check becomes circular (Guardrail 13).
const IDEA = A.idea || null
// QUARANTINE ASSERT (fail loud, zero agents) — the leak vectors into every prompt are APEX and SOURCES,
// not the lens boilerplate. This catches VERBATIM leaks only; paraphrase ("no early signal on progress"
// as an apex clause when the idea IS an early-signal tool) is on the orchestrator — the Phase-0 (a3)
// apex-scrub rule. Post-run this stamps provenance.quarantine='asserted' for the Phase-4 CLR check.
if (IDEA) {
  const leakTerms = [IDEA.statement, IDEA.mechanism].filter(Boolean).map(t => String(t).toLowerCase())
  if ([APEX, SOURCES].some(s => leakTerms.some(t => String(s).toLowerCase().includes(t))))
    throw new Error('QUARANTINE BREACH: the idea leaked into APEX/SOURCES — scrub the pitch before launching')
}
const LENSES = {
  standard: ['funnel/mechanics', 'data-skeptic', 'competitive/external', 'customer-psychology', 'economics/incentives', 'product-eng-constraint'],
  deep:     ['funnel/mechanics', 'data-skeptic', 'competitive/external', 'customer-psychology', 'economics/incentives', 'product-eng-constraint', 'metrics/measurement', 'distribution/discovery', 'org/ownership'],
  'test-plan': ['funnel/mechanics', 'data-skeptic', 'economics/incentives', 'product-eng-constraint'],
}[DEPTH]
const REFUTE_VOTES = { standard: 1, deep: 3, 'test-plan': 1 }[DEPTH]
const LOOP_UNTIL_DRY = DEPTH === 'deep'
// HARD CAPS — without these, dedup misses + loop-until-dry + the refute fan-out compound into a
// runaway agent count (an early Deep run spawned 171 agents vs ~25-30 advertised, and the blowup
// rate-limited and DEGRADED its own refute pass). Cap the three fan-out stages explicitly.
const MAX_DEEPEN = { standard: 12, deep: 20, 'test-plan': 8 }[DEPTH]   // max branches that get a deepen agent
const MAX_REFUTE = { standard: 8,  deep: 14, 'test-plan': 6 }[DEPTH]   // max load-bearing branches sent to the refute panel
const MAX_AUDIT  = { standard: 6,  deep: 10, 'test-plan': 4 }[DEPTH]   // max load-bearing MEASURED nodes sent to the pipeline audit

const BRANCH_SCHEMA = { type:'object', properties:{ branches:{ type:'array', items:{ type:'object',
  properties:{ text:{type:'string'}, stage:{type:'string'},
    kind:{type:'string', enum:['MEASURED','INSTANCE','EXTERNAL','CLAIM','INFERENCE','HYPOTHESIS','FRAMING']},
    grade:{type:'string', enum:['Strong','Mod','Weak']}, cite:{type:'string'},
    validation:{type:'string', enum:['raw','validated','triangulated','contested']} },
  required:['text','stage','kind','grade','cite'] }}}, required:['branches'] }

// 1) BRANCH-SPACE FAN-OUT - one agent per lens, blind to each other
phase('Branch fan-out')
const lensResults = await parallel(LENSES.map(lens => () =>
  agent(
    `You are diagnosing this problem through ONE lens: ${lens}.\n` +
    `APEX (undesirable effect / gap-vs-goal): "${APEX}"\n` +
    `Evidence sources you may use: ${SOURCES}. Use web research (Exa/firecrawl) and read any named files.\n` +
    `Emit candidate WHY-branches under the apex from your lens ONLY. For each: a clear causal statement, ` +
    `the journey stage it sits in, and - critically - its answer-kind (MEASURED/INSTANCE/EXTERNAL/CLAIM/INFERENCE/HYPOTHESIS), ` +
    `a confidence grade (Strong/Mod/Weak), and a citation (n + window + source-registry id for MEASURED). Do not invent numbers; if you can't measure it, mark HYPOTHESIS and name the test. ` +
    `A MEASURED number from a pipeline you have NOT verified (bots in the denominator? does the source's metric DEFINITION match your claim? seasonal window?) is validation:'raw' — say so; only a checked or curated source earns 'validated'. ` +
    `A Δ-over-time claim ("dropped Jan→Jun") without YoY/seasonal normalization is INFERENCE/Weak at best, never MEASURED.`,
    { label:`lens:${lens}`, phase:'Branch fan-out', schema:BRANCH_SCHEMA }
  )))
const allBranches = lensResults.filter(Boolean).flatMap(r => r.branches)

// 2) DEDUP + BUCKET + HARD CAP (plain code - genuinely needs all branches at once)
const merged = rankByLoadBearing(dedupeByMeaning(allBranches)).slice(0, MAX_DEEPEN)  // dedup, rank, THEN cap
log(`${allBranches.length} raw branches -> ${merged.length} after dedup+cap (MAX_DEEPEN=${MAX_DEEPEN})`)

// 3) DEEPEN + GRADE (+ optional loop-until-dry), per branch, no barrier
const deepened = await pipeline(merged,
  b => agent(
    `Branch: "${b.text}" (stage ${b.stage}).\n` +
    `Ask "why does this happen?" ${LOOP_UNTIL_DRY ? 'recursively until you hit bedrock (loop-until-dry)' : 'ONE level down'}.\n` +
    `Sources: ${SOURCES}. Grade EVERY child node (answer-kind + grade + citation). ` +
    `For any child you grade HYPOTHESIS or CLAIM, add a \`gap\` tag: 'in-hand' if one query/read over the sources above would settle it — in that case DON'T punt: run the read NOW and grade it MEASURED instead — 'external' if it needs data not in the sources, 'experiment' if it needs a test. Never leave an in-hand gap as a hypothesis. ` +
    `For any child you grade MEASURED, set validation: 'raw' unless you verified the pipeline (denominator clean? definition matches the claim? window not seasonal?) or the source registry marks it curated/canonical — then 'validated'. Δ-over-time claims without YoY normalization are INFERENCE/Weak, not MEASURED. ` +
    `Return the branch as a node that KEEPS its own top-level kind + grade (+ a lensCount) AND carries a nested children[] tree — the downstream load-bearing filter reads those top-level fields, so do NOT drop them.`,
    { label:`deepen:${b.stage}`, phase:'Deepen + grade', schema: NODE_TREE_SCHEMA }
  ))

// 3b) MEASURABLE-IN-HAND GUARD (fail loud) — a decisive node left HYPOTHESIS/CLAIM when one read over the
//     Phase-1 sources would settle it is the skill's cardinal sin (a live run left "why activators stop before
//     the wall" as HYPOTHESIS when the distribution was one GROUP BY away). Catch in-hand gaps, run a single
//     targeted pass that actually computes them and re-grades MEASURED, and WARN loudly if any survive ungraded.
const inHandGaps = [];
;(function scan(nodes){ (nodes||[]).forEach(n => {
  if (n && ['HYPOTHESIS','CLAIM'].includes(n.kind) && n.gap === 'in-hand') inHandGaps.push(n);
  if (n) scan(n.children);
}); })(deepened.filter(Boolean));
let measurableWarning = '';
if (inHandGaps.length) {
  log(`⚠ ${inHandGaps.length} in-hand gap(s) left ungraded — computing them now (they must not stay HYPOTHESIS).`);
  await parallel(inHandGaps.map(n => () =>
    agent(`This node was left ${n.kind} but is MEASURABLE IN HAND. Run the read over the sources and re-grade.\n` +
          `Node: ${JSON.stringify({ id:n.id, text:n.text })}\nSources: ${SOURCES}.\n` +
          `Return {kind, grade, cite, gap}. Only stay non-MEASURED if the source genuinely lacks it — then set gap:'external'.`,
          { label:`measure:${n.id || '?'}`, phase:'Deepen + grade', schema: REGRADE_SCHEMA })
      .then(r => { if (r) { n.kind = r.kind; n.grade = r.grade || n.grade; n.cite = r.cite || n.cite;
                            n.gap = r.kind === 'MEASURED' ? undefined : (r.gap || 'external'); } })));
  const stillOpen = inHandGaps.filter(n => n.kind !== 'MEASURED' && n.gap === 'in-hand');
  if (stillOpen.length) {
    measurableWarning = `${stillOpen.length} decisive node(s) are MEASURABLE IN HAND but were left ungraded (${stillOpen.map(n => n.id || n.text.slice(0,30)).join('; ')}). Compute them before trusting the verdict — do NOT ship them as a cheapest-test or data-request line.`;
    log('⚠ ' + measurableWarning);
  }
}

// 3c) MEASUREMENT AUDIT — the raw→validated ladder. Field failure mode: three MEASURED/high-confidence
//     nodes were pipeline artifacts (bots in the denominator; an API metric whose DEFINITION didn't match
//     the claim; a seasonal Δ read as a trend) and cost the user 4 of 6 follow-up iterations. The refute
//     pass does NOT catch these — its prompt hunts THIN evidence, and a MEASURED/Strong artifact doesn't
//     look thin. So: audit the PIPELINE of each load-bearing MEASURED node, and TRIANGULATE against a
//     second registry source where one exists. Bounded by MAX_AUDIT, like the other fan-outs.
phase('Measure audit')
const auditPool = []
;(function scanM(ns){ (ns||[]).forEach(n => {
  if (n && n.kind === 'MEASURED' && (n.validation || 'raw') === 'raw') auditPool.push(n)
  if (n) scanM(n.children)
}) })(deepened.filter(Boolean).filter(isLoadBearing))
const toAudit = auditPool.slice(0, MAX_AUDIT)
if (auditPool.length > MAX_AUDIT)
  log(`⚠ measurement audit capped at ${MAX_AUDIT}: ${auditPool.length - MAX_AUDIT} MEASURED node(s) stay validation:'raw' (grade ≤ Mod, cannot be loadBearing).`)
await parallel(toAudit.map(n => () =>
  agent(`Audit the MEASUREMENT PIPELINE behind this node — attack the NUMBER, not the causal logic.\n` +
        `Node: ${JSON.stringify({ text:n.text, cite:n.cite })}\nSource registry + sources: ${SOURCES}\n` +
        `Checklist: (1) denominator pollution — bots/internal/test traffic in the base? (2) definition mismatch — does the source's metric definition match the claim's meaning (placed-vs-paid, sessions-vs-users, gross-vs-net)? (3) window/seasonality — is any Δ-claim YoY-normalized, definition-constant across both endpoints, and above the noise floor for its n? (4) attribution — does the join actually connect this cause to this effect? (5) survivorship — does the extract pre-filter in a biasing way? (6) computation traps — join explosion (row counts before/after joins; COUNT(DISTINCT) for entities), average-of-averages, partial-vs-full period.\n` +
        `THEN TRIANGULATE: if a SECOND independent source in the registry can compute this number, compute it and compare — disagreement in sign, or beyond ~20%, = contested. If no second source exists, triangulate by TRACING 3-5 individual records end-to-end instead (the "6/6 sampled accounts" move).\n` +
        `Red-flag smells that demand a second look: exact round numbers, rates at exactly 0%/100%, a >50% period swing with no known cause, identical values across periods/segments, and a number that PERFECTLY confirms the branch it supports.\n` +
        `Return {validation, triangulated, secondValue?, discrepancy?, evidence}. Stay 'raw' only if the checklist genuinely can't be run against the sources.`,
        { label:`audit:${n.id || n.text.slice(0,24)}`, phase:'Measure audit', schema: AUDIT_SCHEMA })
    .then(a => { if (!a) return
      n.validation = (a.validation === 'validated' && a.triangulated) ? 'triangulated' : a.validation
      if (n.validation === 'contested') { n.grade = 'Weak'; n.cite += ` | CONTESTED vs 2nd source: ${a.discrepancy || a.evidence}` }
      else if (n.validation === 'raw' && n.grade === 'Strong') n.grade = 'Mod'   // unvalidated caps at Mod
    })))

// 3d) RAW-CAP SWEEP (plain code) — the audit only reaches load-bearing nodes; enforce "raw caps at Mod"
//     on EVERY remaining MEASURED node so a non-audited raw/Strong never inflates the census or a chain.
;(function capRaw(ns){ (ns||[]).forEach(n => {
  if (n && n.kind === 'MEASURED' && (n.validation || 'raw') === 'raw' && n.grade === 'Strong') n.grade = 'Mod'
  if (n) capRaw(n.children)
}) })(deepened.filter(Boolean))

// 4) ADVERSARIAL REFUTE - perspective-diverse skeptics try to KILL each load-bearing branch
phase('Refute')
// 'dirty-pipeline' attacks the NUMBER's pipeline (denominator/definition/window/attribution) — ordered FIRST
// for branches resting on MEASURED evidence, because strong-looking numbers are exactly where the classic
// angles don't look (field lesson). Costs no extra agents: it re-orders the angle list, not the vote count.
const REFUTE_LENSES = ['data-says-otherwise','mechanism-broken','survivorship/selection','mis-attribution','dirty-pipeline']
const restsOnMeasured = b => { let f=false; (function w(n){ if(!n||f)return; if(n.kind==='MEASURED')f=true; (n.children||[]).forEach(w); })(b); return f }
const anglesFor = b => (restsOnMeasured(b)
  ? ['dirty-pipeline', ...REFUTE_LENSES.filter(a => a !== 'dirty-pipeline')]
  : [...REFUTE_LENSES.filter(a => a !== 'dirty-pipeline'), 'dirty-pipeline']).slice(0, REFUTE_VOTES + 1)
// LOUD refute-failure guard (a live run silently ran refute on 0 branches because the deepen
// node lost its kind/grade and isLoadBearing filtered everything out — the core value-add vanished
// with no warning). If REFUTE_VOTES>0 and nothing is load-bearing, WARN and surface it in converge.
const lbBranches = deepened.filter(Boolean).filter(isLoadBearing).slice(0, MAX_REFUTE);
let refuteWarning = '';
if (REFUTE_VOTES > 0 && lbBranches.length === 0) {
  refuteWarning = 'REFUTE PASS RAN ON 0 BRANCHES — no independent adversarial verification happened this run (check that deepen nodes carry kind/grade/lensCount for isLoadBearing, and watch for rate-limited deepen agents). Treat every load-bearing node as UN-REFUTED.';
  log('⚠ ' + refuteWarning);
}
const refuted = (REFUTE_VOTES === 0 || lbBranches.length === 0) ? deepened.filter(Boolean) : await pipeline(
  lbBranches,   // cap the refute fan-out too
  branch => { const angles = anglesFor(branch); return parallel(
    angles.map(angle => () =>
      agent(`Try to REFUTE this branch from the "${angle}" angle. ` +
            (angle === 'dirty-pipeline'
              ? `Attack the NUMBERS' pipeline, not the logic: who is in the denominator (bots/internal/test traffic)? does each cited metric's DEFINITION match the claim (placed-vs-paid, sessions-vs-users)? is any Δ-over-time claim seasonal/YoY-normalized? is the attribution join sound? A strong-looking MEASURED node with a dirty pipeline MUST be refuted. `
              : `Default to refuted=true if the evidence is thin or selection-biased. Also ask: would the claim survive segmentation (Simpson/mix-shift can manufacture an aggregate trend), and — given how many branches were searched in parallel this run — is this pattern one of the expected-by-chance findings (multiple comparisons)? `) +
            `Branch: ${JSON.stringify(branch)}. Sources: ${SOURCES}.`,
            { label:`refute:${angle}`, phase:'Refute', schema: VERDICT_SCHEMA }))
  ).then(votes => {
    const kills = votes.filter(Boolean).filter(v => v.refuted)
    if (kills.length > angles.length / 2)
      return { ...branch, role: kills.some(k=>k.demote) ? 'DOWNGRADED' : 'REFUTED', killedBy: kills.map(k=>k.evidence) }
    return branch
  }) }
)

// 5) CONVERGE - collapse to roots + locate THE constraint (needs ALL surviving branches -> barrier)
phase('Converge')
const converged = await agent(
  `Here are the surviving, graded, refute-tested branches (each already carries a nested children[] why-chain):\n${JSON.stringify(refuted)}\n` +
  `1) GROUP them into 3-5 ROOT causes (group ≠ flatten — keep each branch's children[] chain intact under its root). ` +
  `2) Locate THE single system constraint (the root that, if removed, unblocks the most); set its isConstraint:true. ` +
  `3) On the constraint root, attach the FULL nested children[] why-chain from apex down to bedrock (do NOT compress it to a one-line title). ` +
  `4) Say whether it's POLICY/ownership vs TOOLING; if it's a goal-conflict, apply the Evaporating Cloud (name the false assumption). ` +
  `5) List NEGATIVE branches (fixes that backfire) as {text, restsOn:[node ids]}. 5b) Give every root an action AND action's restsOn:[the real node ids the action stands on] — a recommendation must be retractable when its evidence falls. 6) Rank 1-3 CHEAPEST tests by (verdict-movement ÷ cost). ` +
  `6b) Tag every HYPOTHESIS/CLAIM node with gap ∈ in-hand|external|experiment (an in-hand gap is illegal — compute + grade MEASURED instead), then build dataRequest[] from the external/experiment gaps only: each {want, upgrades:[real node ids], from, kind, rank, impact}, ranked, #1 = what would locate the constraint. ` +
  `7) Compute the answer-kind census and write the honest bottom line (verdict vs map). ` +
  `7b) corrections[]: 1-3 one-liners for the claims the refute pass KILLED or DEMOTED, each keeping its killing evidence — the artifact renders these as a "live corrections" banner, often its most-trusted section. ` +
  `8) Set constraint.loadBearing to an id that EXISTS in the tree you are emitting (a real stage-node or constraint-chain node id — do NOT invent one), and ifFalse to what changes if it falls (reference only real ids). ` +
  (refuteWarning ? `IMPORTANT — ${refuteWarning} Say this explicitly in census.bottomLine and treat the verdict as a MAP, not settled. ` : '') +
  (measurableWarning ? `IMPORTANT — ${measurableWarning} Reflect this in census.bottomLine. ` : '') +
  (LEVER_MODE ? `NOTE — the apex is a GROWTH TARGET ("hit N"). Converging ROOT CAUSES is still correct, but do NOT collapse parallel GROWTH PATHS (levers, e.g. acquire-net-new vs migrate-the-base vs raise-conversion) into one — those are ADDITIVE paths, decomposed in a later stage, NOT rival causes to refute away. The constraint is the binding STEP; the levers are how you reach N. ` : '') +
  `Return the FULL tree-JSON object per the schema.`,
  { label:'converge', phase:'Converge', schema: TREE_JSON_SCHEMA, effort:'high' }
)

// 6) DRILL THE CONSTRAINT BRANCH to bedrock — guarantees the ONE branch that matters is a real
//    multi-level chain even if converge flattened it. Runs every tier. This is the adaptive-depth fix:
//    deep recursion is spent where it counts (the constraint), not uniformly across all branches.
phase('Drill constraint')
const cRoot = (converged.roots || []).find(r => r.isConstraint)
// Fire on EMPTY **or FLAT** chains — a depth-1 sibling list under the constraint is the flat-funnel bug too
// (observed live: converge emitted 5 parallel children, the empty-only trigger skipped the drill, and the CLR
// multi-level check failed). When children already exist, the drill RESTRUCTURES them instead of inventing.
const chainDepth = ns => (!ns || !ns.length) ? 0 : 1 + Math.max(...ns.map(n => chainDepth(n.children || [])))
if (!TEST_PLAN && cRoot && chainDepth(cRoot.children || []) < 2) {   // test-plan stops at the map — the ranked tests ARE the deliverable
  const feed = (converged.stages||[]).flatMap(s => s.nodes||[]).filter(n => (cRoot.from||[]).includes(n.id))
  const hasFlat = !!(cRoot.children && cRoot.children.length)
  const chain = await agent(
    `The located system constraint is: "${cRoot.title}".\n` +
    (hasFlat
      ? `Its current children are a FLAT sibling list (no nesting): ${JSON.stringify(cRoot.children)}. RESTRUCTURE these into an explicit nested why-chain — PRESERVE the existing nodes verbatim (keep id/kind/grade/cite/validation/role exactly); add at most 1-2 new linking nodes, graded honestly, with kind/grade/gap values strictly from the schema enums. A parallel support may stay a sibling of the chain node it supports.\n`
      : `Supporting evidence nodes: ${JSON.stringify(feed)}.\n`) +
    `Build the EXPLICIT causal chain UNDER it as a nested children[] tree: each node a declarative graded cause statement ` +
    `("X because Y"), recursing "why does THIS happen?" until you hit BEDROCK (a market fact / deliberate policy / law of ` +
    `the domain / something outside our control — where the next 'why' yields nothing new). 3-6 levels is typical. Grade ` +
    `every node (kind + grade + cite). Return {children:[...]} only.`,
    { label:'drill:constraint', phase:'Drill constraint', schema: NODE_TREE_SCHEMA, effort:'high' })
  if (chain && chain.children && chain.children.length) cRoot.children = chain.children
}

// 6-fix) WIRING FIX (deterministic) — converge has been observed emitting stage TITLES in roots[].from instead
// of node ids, which breaks the viz's edge-lighting and the constraint's "converges from" tally. Map titles
// to that stage's node ids in plain code; leave real ids untouched.
;(converged.roots || []).forEach(r => {
  r.from = (r.from || []).flatMap(f => {
    const st = (converged.stages || []).find(s => s.title === f)
    return st ? (st.nodes || []).map(n => n.id).filter(Boolean) : [f]
  })
})

// 7) VALIDATE constraint.loadBearing / ifFalse against real ids — converge has been observed to
//    emit phantom ids (e.g. "r1-3a"), which silently breaks the viz stress-test. Re-point if invalid.
const allIds = new Set();
;(converged.stages||[]).forEach(s=>(s.nodes||[]).forEach(function w(n){ if(n.id)allIds.add(n.id); (n.children||[]).forEach(w); }))
;(converged.roots||[]).forEach(r=>(r.children||[]).forEach(function w(n){ if(n.id)allIds.add(n.id); (n.children||[]).forEach(w); }))
if (converged.constraint && converged.constraint.loadBearing && !allIds.has(converged.constraint.loadBearing)) {
  const cr = (converged.roots||[]).find(r=>r.isConstraint) || {}
  const repoint = (cr.from||[]).find(id=>allIds.has(id)) || (cr.children&&cr.children[0]&&cr.children[0].id) || null
  log(`⚠ phantom loadBearing id "${converged.constraint.loadBearing}" — re-pointing to ${repoint}`)
  converged.constraint.loadBearing = repoint
}
if (refuteWarning) converged._refuteWarning = refuteWarning  // carried through so the decision-doc/CLR can surface it
if (measurableWarning) converged._measurableWarning = measurableWarning  // in-hand gaps that survived — surface in decision-doc/CLR
// Stamp provenance from the ORCHESTRATOR, not the converge agent — converge has been observed copying a
// stray number (e.g. "171") from context it read into provenance.depth/agents. DEPTH + the real count win.
converged.provenance = { method:'Goldratt CRT', ...(converged.provenance||{}), depth: DEPTH, ...(IDEA ? { mode:'idea-audit', quarantine:'asserted' } : {}) /*, agents: <real count if tracked> */ }
converged.apexType = APEX_TYPE

// 7d) VERDICT STATUS — bind the headline's confidence to the census. Field lesson: the census honestly said
//     "the constraint rests on HYPOTHESIS" while the HTML header looked act-ready, and the reader trusted the
//     header. Computed HERE in plain code, never by an agent — it's arithmetic over the tree, not a judgment.
let _lb = null
;(function findLB(ns){ (ns||[]).forEach(n => { if (n && n.id === (converged.constraint||{}).loadBearing) _lb = n; if (n) findLB(n.children) }) })(
  [...(converged.stages||[]).flatMap(s=>s.nodes||[]), ...(converged.roots||[]).flatMap(r=>r.children||[])])
// A tree with ZERO MEASURED nodes can never be a verdict, whatever the loadBearing node's kind —
// field run 2026-07-08: an INFERENCE/Mod loadBearing slipped past the kind checks and computed
// 'verdict' on a 0%-MEASURED tree ("located by argument, not by data"); the CLR audit had to
// correct it by hand. Guardrails 5 + 10 govern.
let _hasMeasured = false
;(function scanHM(ns){ (ns||[]).forEach(n => { if (n && n.kind === 'MEASURED') _hasMeasured = true; if (n) scanHM(n.children) }) })(
  [...(converged.stages||[]).flatMap(s=>s.nodes||[]), ...(converged.roots||[]).flatMap(r=>r.children||[])])
const _lbWeak = !_lb || ['HYPOTHESIS','CLAIM'].includes(_lb.kind) ||
                (_lb.kind === 'MEASURED' && ['raw','contested'].includes(_lb.validation || 'raw')) ||   // unaudited = raw = map, deliberately
                !_hasMeasured
converged.verdictStatus = (_lbWeak || refuteWarning || measurableWarning || TEST_PLAN) ? 'map' : 'verdict'

// 7d²) CENSUS COUNTS — same rule as verdictStatus: arithmetic over the FINAL tree, never the converge
//      agent's memory. Field lesson (2026-07-04): the agent tallied the whole pre-prune branch pool (60
//      nodes) while the shipped tree had 39 — three artifacts then cited three different censuses. The
//      agent keeps only `bottomLine` (a judgment); the numbers are recomputed here so census, tree, and
//      decision doc can never disagree.
const _tally = {}; let _tot = 0
;(function countK(ns){ (ns||[]).forEach(n => { if (n && n.kind && n.kind !== 'FRAMING') { const k = n.kind.toLowerCase(); _tally[k] = (_tally[k]||0) + 1; _tot++ } if (n) countK(n.children) }) })(
  [...(converged.stages||[]).flatMap(s=>s.nodes||[]), ...(converged.roots||[]).flatMap(r=>r.children||[])])
converged.census = { bottomLine: (converged.census||{}).bottomLine || '',
  ...Object.fromEntries(['measured','instance','external','claim','inference','hypothesis'].map(k => [k, _tot ? Math.round(100*(_tally[k]||0)/_tot) : 0])) }

// 7d³) GRADE-CAP SWEEP over the FINAL tree — converge preserves `validation` but has been observed
//      shipping a contested node at Mod (field run 2026-07-04): the audit-time caps (raw→Mod,
//      contested→Weak) must be re-enforced deterministically after converge rebuilds the tree.
;(function capFinal(ns){ (ns||[]).forEach(n => { if (n && n.kind === 'MEASURED') {
  if ((n.validation||'raw') === 'raw' && n.grade === 'Strong') n.grade = 'Mod'
  if (n.validation === 'contested') n.grade = 'Weak' }
  if (n) capFinal(n.children) }) })(
  [...(converged.stages||[]).flatMap(s=>s.nodes||[]), ...(converged.roots||[]).flatMap(r=>r.children||[])])

// 7b) LEVER DECOMPOSITION + SIZING — TARGET APEX ONLY. A constraint tree finds the ONE bottleneck; a
//     "hit N by date" goal ALSO needs the ADDITIVE paths to N, sized, so the output is a portfolio that
//     sums to the number — not a lone bottleneck. The L2-suppression fix lives here: levers are PARALLEL
//     paths, NOT competing causes — a lever may be deprioritized (role:'dropped' + droppedReason) but NEVER
//     silently vanish the way a real growth lever did when the converge step refuted everything down to one.
if (LEVER_MODE) {
  phase('Levers + size')
  const lev = await agent(
    `The apex is a GROWTH TARGET: "${APEX}". The constraint tree below found the binding root cause; now ` +
    `decompose the PATHS TO THE NUMBER.\nConstraint + roots + evidence: ${JSON.stringify({ roots:converged.roots, constraint:converged.constraint, stages:converged.stages })}\nSources: ${SOURCES}.\n` +
    `1) Enumerate the ADDITIVE levers (distinct ways to add to N — e.g. acquire-net-new via X / migrate-the-existing-base / raise-conversion-of-segment-Y). ` +
    `2) SIZE each: volume × conversion → contribution toward N, with the basis (MEASURED where the data supports it, else gap-tagged external/experiment). ` +
    `3) Tag each Own/Win/Buy + effort(low/med/high); set bindsConstraint:true on the lever the located constraint sits in. ` +
    `4) role each primary|secondary|dropped — and for EVERY dropped lever give a droppedReason. A lever is a parallel path: deprioritize it with a reason, never delete it silently. ` +
    `5) sizing: sum the contributions, state gapToTarget vs N, and an honest bottomLine — do the levers CREDIBLY sum to N, or does the number lean on one unproven lever?`,
    { label:'levers+size', phase:'Levers + size', schema: LEVERS_SCHEMA, effort:'high' })
  if (lev) { converged.levers = lev.levers || []; converged.sizing = lev.sizing || null }
}

// 7b²) IDEA FIT-CHECK — IDEA AUDIT ONLY. The finished tree finally meets the quarantined idea. Field
//      origin (2026-07): a user brought a product idea; the tree — run on the problem the idea presupposed —
//      located a policy/ownership constraint the tool couldn't touch, and the honest kill itself needed
//      grading (the WTP call is a hypothesis with a test, not a pronouncement). Two agents: map the idea
//      onto the tree, then one skeptic attacks the fit verdict itself. Runs BEFORE narrate so the memo
//      can state the idea verdict in plain words. In TEST_PLAN the fit-check is DEFERRED, not skipped —
//      a fit verdict over an unsettled map rests on hypotheses; tell the user it fires in the Phase-6
//      loop once the decisive tests come back. (Scout-gate ship-direct: run this same pair against the
//      scout consensus {constraint, decisiveEvidence} instead of a full tree.)
if (IDEA) converged.idea = IDEA            // persist the question UNCONDITIONALLY — a dead fit agent or a
                                           // test-plan run must never silently delete what the user asked
if (IDEA && TEST_PLAN) {
  converged.ideaFitDeferred = 'test-plan: the fit-check fires in the Phase-6 loop once the decisive tests land — a fit verdict over an unsettled map would rest on hypotheses'
  log('ℹ Idea Audit × Test-plan: fit-check DEFERRED to the update loop (idea saved in the output JSON).')
}
if (IDEA && !TEST_PLAN) {
  phase('Idea fit')
  const fit = await agent(
    `A finished, evidence-graded Why Tree is below. Now judge this IDEA against it — the idea was deliberately ` +
    `hidden from the agents that built the tree, so the tree owes it nothing.\nIDEA: ${JSON.stringify(IDEA)}\n` +
    `TREE: ${JSON.stringify({ roots:converged.roots, constraint:converged.constraint, stages:converged.stages, negatives:converged.negatives })}\n` +
    `Emit ideaFit: 1) addresses:[REAL node ids the idea would actually change]. 2) verdict — dissolves-constraint ` +
    `(removes/moves the located constraint) | relieves-symptom (real nodes, not the constraint) | misses-constraint ` +
    `(refuted/peripheral nodes) | negative-branch (run the idea forward like any fix — it would backfire). 3) why + ` +
    `restsOn:[the real node ids the verdict stands on]; a check standing on DIFFERENT nodes than the verdict carries its ` +
    `own restsOn. 4) policyToolMismatch — if the constraint is POLICY/ownership and ` +
    `the idea is a TOOL that leaves ownership untouched, flag it: a tool cannot dissolve a policy constraint. 5) absorbedPain — ` +
    `acute (escalating, budgeted-to-fix) vs chronic-absorbed (the system built compensating routines; the cost was internalized ` +
    `years ago → low willingness-to-pay even when the pain is real and widespread) vs unknown; this is a GRADED claim ` +
    `(kind/grade/cite, usually a HYPOTHESIS with gap:'experiment') plus the TEST that would settle it (pre-sell, budget-line ` +
    `check, adjacent purchases) — NEVER an ungraded pronouncement. 6) segmentFork — this tree diagnosed ONE system: where does ` +
    `the idea die / where might it live (kind HYPOTHESIS, gap external, + test)? 7) redirect — what WOULD dissolve the ` +
    `constraint; the tree's reframed problem is the alternative idea space.`,
    { label:'idea-fit', phase:'Idea fit', schema: IDEA_FIT_SCHEMA, effort:'high' })
  if (!fit) {
    converged.ideaFitError = 'IDEA FIT-CHECK FAILED — the fit agent died; the idea was never judged. Re-run via the Phase-6 idea-check.'
    log('⚠ ' + converged.ideaFitError)
  } else {
    // Phantom-id REPAIR (mirror the loadBearing re-point, never log-and-ship): drop ids that don't exist,
    // say so in `why`, and if the verdict's support empties out it is contested by construction.
    const scrub = ids => (ids||[]).filter(id => allIds.has(id))
    const dropped = [...(fit.addresses||[]), ...(fit.restsOn||[])].filter(id => !allIds.has(id))
    fit.addresses = scrub(fit.addresses); fit.restsOn = scrub(fit.restsOn)
    if (dropped.length) { fit.why += ` ⚠ ${dropped.length} supporting id(s) did not resolve and were dropped (${dropped.join(', ')}).`
                          log(`⚠ idea-fit phantom id(s) dropped: ${dropped.join(', ')}`) }
    // Fit skeptic — NO default-to-refute here: in a map-status tree the support is always thin, so that
    // default would fire every run and an always-amber badge carries zero information. Thinness is already
    // on the badge (verify-first); the skeptic must name a SPECIFIC error to kill.
    const fitRefute = await agent(
      `Try to REFUTE this idea-fit verdict — argue the side it did NOT take (a kill: could the idea in fact reach the ` +
      `constraint, or live in a nearby segment? a pass: does the fit rest on refuted nodes or wishful sizing?). ` +
      `Set refuted=true ONLY if you can name the specific node, segment, or mechanism the verdict got wrong — ` +
      `thin evidence alone is NOT a refutation (the badge already shows it).\n` +
      `ideaFit: ${JSON.stringify(fit)}\nConstraint: ${JSON.stringify(converged.constraint)}`,
      { label:'refute:idea-fit', phase:'Idea fit', schema: VERDICT_SCHEMA })
    // Record ALL THREE outcomes — the artifact must distinguish "attacked and lost" from "never attacked".
    fit.refutation = fitRefute ? (fitRefute.refuted ? { contested: fitRefute.evidence }
                                                    : { survived: fitRefute.evidence })
                               : { skipped: 'fit skeptic failed to run — the verdict is UN-REFUTED' }
    if (fit.refutation.contested) fit.contested = fit.refutation.contested   // keep BOTH sides visible
    if (!fit.restsOn.length) { fit.contested = (fit.contested ? fit.contested + ' | ' : '') + 'every supporting node id failed to resolve — the verdict has no surviving evidence anchor' }
    converged.ideaFit = fit
  }
}

// 7c) NARRATE — turn the finished tree into a short memo a human reads FIRST (rendered at the TOP of the
//     HTML so the artifact self-explains before anyone drills the tree). One cheap agent; plain prose.
//     SKIPPED in test-plan mode ("no drill, no narrate, no HTML") — the orchestrator writes the markdown
//     map's short intro itself; minimal mode stays minimal.
if (!TEST_PLAN) {
phase('Narrate')
const narr = await agent(
  `Write a tight executive NARRATIVE that makes this Why Tree comprehensible at a glance — the reader sees ` +
  `this memo first, then drills the tree for evidence. Full tree: ${JSON.stringify(converged)}.\n` +
  `Return {sections:[{heading, body}]} — 3 to 7 sections of plain prose (NOT bullets), each body 2-5 sentences, in this order: ` +
  `the problem (the apex in one line); what the evidence FORCES (the verdict/reframe); what we know (the load-bearing graded findings, de-whaled); ` +
  `the binding constraint${LEVER_MODE ? ' AND the lever portfolio (which additive paths sum to the target, and the honest gap)' : ''}${converged.ideaFit ? '; the idea on trial (the fit verdict in plain words, its strongest counter-argument if contested, and the redirect)' : ''}; ` +
  `what to do / what NOT to do (the negative branches); and the honest risk (the loadBearing node + what flips if it is wrong${'' }${''}). ` +
  ((converged._refuteWarning||converged._measurableWarning) ? `Surface this caveat plainly: ${esc0(converged._refuteWarning||'')} ${esc0(converged._measurableWarning||'')}. ` : '') +
  `Match the tree exactly — do NOT invent facts or numbers not in it.`,
  { label:'narrate', phase:'Narrate', schema: NARRATIVE_SCHEMA })
converged.narrative = (narr && narr.sections) || []
}

return converged
```

*(`esc0` is a trivial string-guard, `s => (s||'').replace(/[`$]/g,' ')`, so a warning never breaks the template literal — define it near the top of the script.)*

## Notes for adapting

- **Pre-compute the hard evidence ONCE in Phase 1 and pass it as a frozen brief** into every lens prompt (e.g., real DB counts) with "do NOT re-query". This grounds nodes as MEASURED instead of HYPOTHESIS and stops N parallel agents from re-hammering the same DB/API. (A real data-grounded run: the lens agents shared one pre-pulled metrics brief; none re-hit the DB.)
- Helper functions to fill in when you instantiate (keep schemas strict so agents retry on mismatch):
  - `dedupeByMeaning(branches)` — **must actually merge near-duplicates.** Use **token-set-ratio ≥ ~0.55** on normalized text (lowercase, strip punctuation/stopwords), keep the best-graded representative per group, union its `cite`s. **Do NOT use a sorted 6-word prefix key — it's too strict and merges nothing** (a live run went 33→33, and only the cap reduced it, which defeats the point of deduping before the cap).
  - `rankByLoadBearing(branches)` — sort so the cap keeps the important ones: MEASURED/Strong and nodes many lenses surfaced first; FRAMING / single-lens / Weak last. The `.slice(0, MAX_DEEPEN)` then drops the tail safely. **`log()` how many were dropped** — never silently truncate.
  - `isLoadBearing(branch)` — true if the branch is a plausible constraint candidate (not FRAMING, grade ≥ Mod, or flagged by ≥2 lenses). **CRITICAL: this reads the branch's TOP-LEVEL `kind`/`grade`/`lensCount` — the deepen agent MUST propagate those up to the returned branch node, not bury them only inside `children[]`. If it doesn't, `isLoadBearing` returns 0 for everything and the refute pass SILENTLY does nothing** (observed live). The refute-failure guard above turns that silence into a loud warning, but the real fix is propagating the metadata.
  - `NODE_TREE_SCHEMA` = a single branch object that REQUIRES top-level `kind`/`grade` (+ optional `lensCount`, + optional `gap` on HYPOTHESIS/CLAIM nodes, + optional `validation` on MEASURED nodes) and a recursive `children:[NODE]` (used by deepen + the constraint drill); `VERDICT_SCHEMA` = `{refuted:boolean, demote:boolean, evidence:string}`; `REGRADE_SCHEMA` = `{kind:enum(answer-kinds), grade:string, cite:string, gap?:enum('in-hand','external','experiment')}` (used by the measurable-in-hand guard's re-grade pass); `AUDIT_SCHEMA` = `{validation:enum('validated','contested','raw'), triangulated:boolean, secondValue?:string, discrepancy?:string, evidence:string}` (`validation` + `evidence` required — used by the measurement audit); `TREE_JSON_SCHEMA` = the strict schema above (its `roots[].children` is what makes the tree multi-level — do not drop it).
  - `LEVERS_SCHEMA` (target apex only) = `{levers:[{id, name, path, sizing:{volume, conversion, contribution, basis}, ownWinBuy:enum('own','win','buy'), effort:enum('low','med','high'), bindsConstraint?:boolean, role:enum('primary','secondary','dropped'), droppedReason?}], sizing:{target, byDate, sumOfLevers, gapToTarget, bottomLine}}` — `levers` + `sizing.bottomLine` required; every `dropped` lever MUST carry a `droppedReason` (the no-silent-suppression rule).
  - `IDEA_FIT_SCHEMA` (idea audit only) = `{verdict:enum('dissolves-constraint','relieves-symptom','misses-constraint','negative-branch'), addresses:[string], why:string, restsOn:[string], policyToolMismatch:{flag:boolean, note}, absorbedPain:{state:enum('acute','chronic-absorbed','unknown'), kind:enum(answer-kinds), grade:enum('Strong','Mod','Weak'), cite, test, gap?, restsOn?:[string]}, segmentFork:{diesWhere, livesWhere, kind:enum(answer-kinds), gap, test, restsOn?:[string]}, redirect:{reframedProblem, whatWouldDissolve}}` — REQUIRED: `verdict` + `addresses` + `why` + `restsOn` at top level, **`absorbedPain` (with `state` + `kind` + `test`) and `segmentFork` (with `diesWhere` + `livesWhere` + `kind` + `test`)** — Guardrail 14 is enforced HERE, in the schema, not just in prose: an ungraded idea-kill must fail validation, not slip through as an optional field. A check standing on different nodes than the verdict carries its own `restsOn` (the orphan sweep reads both levels). `refutation` `{contested|survived|skipped}` is stamped by the orchestrator, never emitted by the fit agent.
  - `NARRATIVE_SCHEMA` = `{sections:[{heading, body}]}` (both required) — the top-of-HTML memo (3-7 short prose sections).
  - `esc0` = `s => (s||'').replace(/[`$]/g,' ')` — strips backticks/`$` so a warning string spliced into a template literal can't break it (used when passing `_refuteWarning`/`_measurableWarning` into the narrate prompt).
- **The `gap` tag is the spine of the data-honesty model.** Every HYPOTHESIS/CLAIM node carries one: `in-hand` (one read over the Phase-1 sources settles it → the measurable-in-hand guard computes it and re-grades MEASURED; it must NEVER survive to the output), `external` (needs data we don't have), `experiment` (needs a test). `dataRequest[]` is built from the `external`/`experiment` gaps only — it's the "what I need from you to turn this map into a verdict" deliverable, the generalized ROI-pattern ("I lack the data, but here are the exact numbers that answer it"). Keep it distinct from `tests[]`: `tests[]` are fork-deciding experiments, `dataRequest[]` is the ranked missing-evidence ask (they may overlap; a `dataRequest` item may BE a cheapest-test). Interaction with the user happens only at the edges — the Phase-0 depth gate, the Phase-1 pre-flight data gate (+ interview-me), the Phase-1.5 scout-gate routing, and this Data Request / the Phase-6 test loop — never mid-run. (An `ask-owner` dataRequest item is just the `external`-gap flavor whose "source" is a person's head.)
- **Standard** = 6 lenses, 1-vote refute, one-level deepen + measurement audit + constraint drill. **Deep** = 9 lenses, 3-vote refute, loop-until-dry + wider audit + constraint drill. **Test-plan** = 4 lenses, 1-vote refute, audit, NO drill — the deliverable is the branch-map + ranked cheapest-tests + Data Request as *markdown* (skip Phase 3's HTML machinery entirely); `verdictStatus` is forced to `map` because the decisive sources are human-only. (Quick was REMOVED — for a cheap gut-check, ask a single agent; this skill only earns its cost when one mind can confidently go wrong. Test-plan is not Quick: it's a different output *shape*, not a cheaper verdict.)
- **The source registry rides inside `SOURCES`.** From Phase 1, include each source as `{id, tier: canonical|curated|raw, caveats}` in the SOURCES string, so lens/deepen agents can cite source ids and the audit agents know what counts as a second independent pipeline (and which source the business already trusts). The registry + cleanliness gate happen at the ORCHESTRATOR level before launch — zero agents; the audit pass only covers what the gate couldn't settle.
- **Update re-entry (amend, don't re-derive).** When the user returns with test results, do NOT rerun this workflow. Load `why-tree-output.json`, re-grade the touched nodes in place (HYPOTHESIS→MEASURED with the new cite + `validation`; or `contested`; or role `REFUTED` with the killing evidence), recompute `census` + `verdictStatus` in plain code, run the **orphan sweep** (any action/negative/`ideaFit` claim whose `restsOn` includes a node that died or went contested is flagged ORPHANED — re-derive or retract, never leave it standing), and re-run ONLY the converge(+drill) step if the constraint's `loadBearing` node fell. 0-3 agents per loop. The anti-contamination rule (never read a prior verdict) applies to re-DERIVING a fresh tree, not to amending a live one. **Post-hoc idea-check re-entry:** on *"here's my idea — would it have helped?"* against a finished tree, load the saved JSON and run ONLY the 7b² fit-check pair (+2 agents) over it, then splice `idea` + `ideaFit` into the styled HTML and append the idea section to the decision doc — zero re-derivation. A saved JSON carrying `ideaFitDeferred` (a test-plan Idea Audit) means exactly this re-entry is OWED once the decisive tests land — the idea is already in the JSON; don't make the user re-supply it.
- **Apex type drives the output shape** (set `args.apexType` from Phase 0). `symptom` → the classic constraint tree (no lever stage). `target` ("hit N by date") → the same tree PLUS the lever-decomposition stage (`levers[]` + `sizing`): the additive paths to N, sized, so the verdict is "the bottleneck AND whether the levers sum to the number." The lever stage is **+1 agent** (decompose; narrate is already in the core count's tail). **Idea Audit adds +2** (fit + fit-skeptic), also outside the core count. The suppression rule (a lever may be `dropped` with a `droppedReason`, never silently cut) is the generalization of a real run that lost a whole growth lever when converge refuted everything down to one.
- **`narrative[]` runs for both apex types but is SKIPPED in test-plan mode** (+1 agent otherwise): a final memo the HTML renders at the top, so the single artifact explains itself before anyone opens the tree. In test-plan the orchestrator writes the markdown intro itself.
- **Honest agent count** = lenses + deepen(≤`MAX_DEEPEN`) + audit(≤`MAX_AUDIT`) + refute(≤`MAX_REFUTE` × (`REFUTE_VOTES`+1)) + converge + drill + narrate. The **refute fan-out is the multiplier**: Standard ≈ **22-40**, Deep ≈ **32-60+**, Test-plan ≈ **10-18**. (A real Standard field run: 26 agents ≈ 833k output tokens, ~32k/agent.) The "~12-16 / ~25-30" figures in older docs predate the caps, the drill and the audit steps — SKILL.md's gate now quotes the realistic ranges. The caps bound the worst case (no 171-blowup); they don't make it cheap.
- For data-grounded problems, name the file paths / DB tables explicitly in `SOURCES` so the deepen/refute agents read them (this is what produced real refutations like "6/6 sampled accounts had a 2nd seat" in the source case).
- Save `converged` to `why-tree-output.json` before rendering, so a render failure never loses the analysis.
- If the user set a token budget, gate the deep loop on `budget.remaining()`.
