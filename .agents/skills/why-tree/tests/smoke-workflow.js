// T3 · Smoke workflow — BOUNDED token cost (~4 small agents, reasoning-only, NO web).
// Proves the real Workflow engine fans out and returns a schema-valid tree end to end.
// Run via the Workflow tool ONLY on explicit request. It is deliberately tiny:
//   3 reasoning-only lenses + 1-level deepen + NO refute + 1 converge.
// For a real diagnosis use the full blueprint in references/workflow-template.md at Standard/Deep depth.
export const meta = {
  name: 'why-tree-smoke',
  description: 'Tiny bounded smoke test of the Why Tree engine (no web, ~4 agents)',
  phases: [{ title: 'Fan-out' }, { title: 'Converge' }],
}

const APEX = args?.apex || 'Why do most New Year resolutions fail by February?'  // known-cold toy problem
const LENSES = ['behavior/habit-design', 'incentives', 'measurement']            // only 3
const NO_WEB = 'Do NOT use web search or any external tools. Reason from general knowledge only — this is a plumbing smoke test, not a real diagnosis.'

const BRANCHES = { type:'object', properties:{ branches:{ type:'array', items:{ type:'object',
  properties:{ text:{type:'string'}, stage:{type:'string'}, kind:{type:'string'}, grade:{type:'string'}, cite:{type:'string'} },
  required:['text','stage','kind','grade'] }}}, required:['branches'] }

// Strict shape so the agent emits RENDER-READY tree-JSON (matches assets/tree-template.html).
// Loose schemas let the agent invent its own structure -> won't render. (Caught by the first smoke run.)
const NODE = { type:'object', properties:{
  id:{type:'string'}, text:{type:'string'},
  kind:{type:'string', enum:['MEASURED','INSTANCE','EXTERNAL','CLAIM','INFERENCE','HYPOTHESIS','FRAMING']},
  grade:{type:'string', enum:['Strong','Mod','Weak']},
  role:{type:'string'}, cite:{type:'string'} }, required:['text','kind','grade'] }
const TREE = { type:'object', properties:{
  apex:{type:'string'},          // SHORT undesirable-effect STATEMENT (<=140 chars), NOT the answer
  verdict:{type:'string'},       // the one-line answer / headline
  stages:{ type:'array', items:{ type:'object', properties:{
    id:{type:'string'}, title:{type:'string'}, crosscut:{type:'boolean'},
    nodes:{ type:'array', items:NODE } }, required:['title','nodes'] }},
  roots:{ type:'array', items:{ type:'object', properties:{
    id:{type:'string'}, title:{type:'string'}, from:{type:'array', items:{type:'string'}},
    action:{type:'string'}, isConstraint:{type:'boolean'} }, required:['id','title'] }},
  constraint:{ type:'object', properties:{
    rootId:{type:'string'}, type:{type:'string'}, statement:{type:'string'}, unlocated:{type:'string'} },
    required:['rootId','statement'] },
  negatives:{ type:'array', items:{type:'string'} },
  tests:{ type:'array', items:{ type:'object', properties:{
    test:{type:'string'}, decides:{type:'string'}, rank:{type:'string'} }, required:['test','decides'] }},
  census:{ type:'object', properties:{
    measured:{type:'number'}, inference:{type:'number'}, hypothesis:{type:'number'},
    instance:{type:'number'}, claim:{type:'number'}, external:{type:'number'}, bottomLine:{type:'string'} },
    required:['bottomLine'] } },
  required:['apex','verdict','stages','roots','constraint','census'] }

phase('Fan-out')
const lensOut = await parallel(LENSES.map(lens => () =>
  agent(`Lens: ${lens}. Apex: "${APEX}". ${NO_WEB}\n` +
        `Emit 2-3 candidate WHY-branches. Tag each with answer-kind (MEASURED/INFERENCE/HYPOTHESIS/...), a grade, a stage, and a one-line cite.`,
        { label:`lens:${lens}`, phase:'Fan-out', schema:BRANCHES, effort:'low' })))
const branches = lensOut.filter(Boolean).flatMap(r => r.branches)

phase('Converge')
const tree = await agent(
  `Branches:\n${JSON.stringify(branches)}\n${NO_WEB}\n` +
  `Keep apex a SHORT statement (not a question); put the one-line answer in verdict. ` +
  `Bucket into stages, collapse to 3-5 roots, name THE single constraint, list negatives + 1-2 cheapest tests, ` +
  `and compute the answer-kind census with an honest bottom line. Return the full tree-JSON.`,
  { label:'converge', phase:'Converge', schema:TREE, effort:'low' })

return tree   // save to tests/out/smoke-tree.json, then render with build_fixtures.py
