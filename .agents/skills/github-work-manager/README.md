# github-work-manager

Portable Codex skill for evidence-based, single-issue GitHub work. Requires Codex skill discovery and `gh` for live GitHub operations. Python scripts use only the standard library and accept snapshots/ledgers; no script makes network calls or mutates GitHub.

## Install

Copy the `github-work-manager/` folder to a Codex skills search path (for example `~/.codex/skills/github-work-manager/`) or a repository's `.agents/skills/github-work-manager/`. Restart/reload skill discovery if needed. Keep the whole folder together. In any GitHub repository, invoke `$github-work-manager PLAN owner/repo <spec-or-issue>` or give one of the mode commands below.

## Invocation examples

- `PLAN owner/repo docs/proposal.md` — inspect and draft atomic issues; wait for approval before publishing.
- `NEXT owner/repo` — select at most one READY task using actual dependency and PR state.
- `EXECUTE owner/repo#123` — implement the approved issue and stop at PR_READY.
- `VERIFY owner/repo#123 owner/repo#456` — criterion-level PR verification.
- `REVIEW owner/repo#123 <review feedback>` — classify feedback and correct the same PR when authorized.
- `RECONCILE owner/repo` — read-only audit.
- `CLOSE owner/repo#123` — close only after merge and evidence gates.

## Helpers

```sh
python3 scripts/preflight.py --target main
python3 scripts/issue-status.py snapshot.json
python3 scripts/evidence.py ledger.json
python3 -m unittest discover -s tests -v
```

`issue-status.py` expects a GitHub-derived JSON snapshot with `metadata_available`, `global_wip_limit` (default 1), `active_tasks`, and `issues`. Each issue includes `state`, `kind`, `repo_active`, `active_pr`, `criteria_sufficient`, `cross_repo_scope`, `already_implemented`, `prerequisites_integrated`, `dependencies` (list or null for unavailable), `priority` (lower is higher), `unlocks`, and `created_at`. Dependency entries need `state: done` and `integrated: true`. Null metadata fails closed. Fetch this data once per invocation using `gh` or supported API/MCP; CLI support for issue dependency relationships varies by installation.

Evidence ledgers require issue, base revision, integration branch, PR, head revision, criteria, checks, review status, and disposition. Criteria statuses are PASS/FAIL/UNVERIFIED; check outcomes PASS/FAIL/SKIP/UNAVAILABLE. DONE also requires merge SHA, all criteria and required checks PASS, clear review status, and evidence URL.

## Dry-run: isolated sample issue lifecycle

No live GitHub calls are made. Sample issue `fixture/repo#7` is an atomic task with AC-1, a passing focused test, no dependencies, and no active PR.

1. PLAN drafts the issue; approval is represented by a fixture approval event, not a GitHub mutation.
2. NEXT on fixture snapshot selects only `#7` as READY. A second active task causes NO_SELECTION.
3. EXECUTE preflight sees a clean fixture worktree, implements one behavior and test, and records PR `#11` at head `h1`; outcome PR_READY.
4. VERIFY finds AC-1 evidence and required CI PASS on `h1`; outcome verified. No production code is changed by the helper test suite.
5. REVIEW classifies a correctness defect as in-scope, adds correction commit `h2` to PR `#11`, reruns focused tests; an enhancement suggestion is recorded for triage.
6. CLOSE is initially UNVERIFIED while PR is open. After fixture merge SHA `m1`, required tests pass on the merge, evidence URL is recorded, and review is clear, CLOSE can produce DONE. It does not execute another issue.

The lifecycle rules are exercised as isolated fixture tests, not a live GitHub integration test.

## Limitations

- GitHub issue dependency APIs and CLI exposure differ; if actual relationship data cannot be queried, NEXT returns UNKNOWN/no selection. This skill does not substitute textual links for blockers.
- Helpers consume explicit snapshots and do not retrieve GitHub records or detect implementation from source. The agent must gather evidence through `gh`, repository inspection, and CI views.
- Codex model switching and Goal invocation are environment-dependent. Model routing recommends only discoverable options and makes no switching claim.
- Project state fields and labels are repository-configured; map them explicitly.
