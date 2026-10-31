// T2 · Plumbing test — ZERO LLM tokens.
// Validates the workflow orchestration wiring (fan-out -> dedup -> deepen -> converge)
// flows data correctly and emits SCHEMA-VALID tree-JSON the visualization accepts.
// Agents are STUBBED with canned data; this tests the pipes + schema, not agent intelligence.
//   run:  node tests/plumbing.test.mjs
import { writeFileSync, mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
const __dir = dirname(fileURLToPath(import.meta.url));

// ---- stub the Workflow runtime hooks ----
const trace = [];
const phase = t => trace.push('phase:' + t);
const log   = m => trace.push('log:' + m);
async function parallel(thunks){ return Promise.all(thunks.map(t => Promise.resolve().then(t).catch(() => null))); }
async function pipeline(items, ...stages){
  return Promise.all(items.map(async (it, i) => {
    let v = it;
    for (const s of stages){ try { v = await s(v, it, i); } catch { return null; } }
    return v;
  }));
}

// ---------- the orchestration under test (mirrors references/workflow-template.md) ----------
const LENSES = ['funnel', 'data-skeptic', 'economics'];

phase('Branch fan-out');
const lensResults = await parallel(LENSES.map(lens => async () => ({
  branches: [
    { id: lens[0].toUpperCase(), text: `[${lens}] a cause this lens sees`, stage: 'Stage 1 · Arrive', kind: 'MEASURED', grade: 'Mod', cite: 'canned n=10' },
    { text: 'shared cross-cutting cause (overlaps across lenses)', stage: 'Cross-cutting', kind: 'HYPOTHESIS', grade: 'Weak', cite: 'canned' },
  ],
})));
const all = lensResults.filter(Boolean).flatMap(r => r.branches);

// dedup (plain code) — collapse the overlapping "shared" branch the 3 lenses all emitted
const seen = new Set();
const merged = all.filter(b => { const k = b.text.replace(/^\[[^\]]+\]\s*/, ''); if (seen.has(k)) return false; seen.add(k); return true; });
log(`${all.length} raw -> ${merged.length} after dedup`);

phase('Deepen + grade');
const deepened = (await pipeline(merged, async b => ({
  ...b, children: [{ text: b.text + ' — why?', kind: 'INFERENCE', grade: 'Weak', cite: 'canned one level' }],
}))).filter(Boolean);

// (refute skipped in smoke)
phase('Converge');
const tree = assembleTree('Smoke: does the plumbing flow end to end?', deepened);

// ---------- assemble + validate ----------
function assembleTree(apex, branches){
  const byStage = {};
  branches.forEach(b => (byStage[b.stage] ||= []).push(b));
  const stages = Object.entries(byStage).map(([title, nodes], i) => ({
    id: 'S' + i, title, crosscut: /cross/i.test(title), nodes,
  }));
  return {
    apex,
    provenance: { method: 'Goldratt CRT', depth: 'plumbing-stub', agents: LENSES.length, date: 'test' },
    stages,
    roots: [
      { id: 'R0', title: 'Stubbed system constraint (policy)', from: branches.map(b => b.id).filter(Boolean), action: 'name a DRI', isConstraint: true },
      { id: 'R1', title: 'Stubbed secondary root', from: [], action: 'fix surface' },
    ],
    constraint: { rootId: 'R0', type: 'policy', statement: 'Located (stub).', unlocated: 'Mechanism unlocated (stub).' },
    negatives: ['stubbed backfire'],
    tests: [{ test: 'the one cheap query', decides: 'forks the verdict', rank: '#1' }],
    census: { measured: 0.4, inference: 0.4, hypothesis: 0.2, instance: 0, claim: 0, external: 0, bottomLine: 'stub census' },
  };
}

function validate(t){
  const e = [];
  const KINDS = ['MEASURED','INSTANCE','EXTERNAL','CLAIM','INFERENCE','HYPOTHESIS','FRAMING'];
  if (typeof t.apex !== 'string' || !t.apex) e.push('apex missing');
  if (!Array.isArray(t.stages) || !t.stages.length) e.push('stages missing');
  const walk = n => {
    if (!n.text) e.push(`node ${n.id||'?'} has no text`);
    if (!KINDS.includes(n.kind)) e.push(`node ${n.id||n.text} bad kind: ${n.kind}`);
    if (!n.grade) e.push(`node ${n.id||n.text} no grade`);
    (n.children||[]).forEach(walk);
  };
  (t.stages||[]).forEach(s => { if (!s.title) e.push('stage no title'); (s.nodes||[]).forEach(walk); });
  if (!Array.isArray(t.roots) || !t.roots.length) e.push('roots missing');
  const cons = (t.roots||[]).filter(r => r.isConstraint);
  if (cons.length !== 1) e.push(`must be exactly ONE constraint root, found ${cons.length}`);
  if (cons[0] && t.constraint?.rootId !== cons[0].id) e.push('constraint.rootId mismatch');
  if (!t.census || typeof t.census.bottomLine !== 'string') e.push('census.bottomLine missing');
  return e;
}

const errors = validate(tree);
const dedupWorked = merged.length < all.length;
const allGraded = tree.stages.every(s => s.nodes.every(n => n.kind && n.grade));

mkdirSync(join(__dir, 'out'), { recursive: true });
writeFileSync(join(__dir, 'out', 'from-plumbing.json'), JSON.stringify(tree, null, 2));

console.log('--- T2 plumbing test ---');
console.log(`fan-out:  ${LENSES.length} lenses -> ${all.length} raw branches`);
console.log(`dedup:    ${all.length} -> ${merged.length}  ${dedupWorked ? 'OK' : 'FAIL (no overlap collapsed)'}`);
console.log(`deepen:   ${deepened.length} branches each got children`);
console.log(`graded:   every node has kind+grade  ${allGraded ? 'OK' : 'FAIL'}`);
console.log(`converge: exactly one constraint root  ${errors.some(x=>x.includes('constraint')) ? 'FAIL' : 'OK'}`);
console.log(`schema:   ${errors.length === 0 ? 'OK' : 'FAIL'}`);
console.log(`output:   tests/out/from-plumbing.json (render with build_fixtures.py)`);
if (errors.length){ console.log('\nERRORS:\n - ' + errors.join('\n - ')); process.exit(1); }
console.log('\nPASS');
