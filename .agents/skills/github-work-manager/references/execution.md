# EXECUTE

Inputs: exact owner/repo and issue number. Fetch the issue, direct dependency state, relevant PRs, branch/base, and checks once; reuse that snapshot during this invocation. Confirm READY: executable, explicit ACs and verification, blockers integrated into target branch, no active implementation PR, no other active repo task, WIP available, and actual dependency data known.

Before editing inspect current branch, target branch, tracked/untracked status, worktrees, and matching branches/PRs. Preserve existing changes. Stop BLOCKED if safety is unclear. Prefer a clean dedicated worktree. Never base on an unmerged feature branch. Confirm prerequisites' code is in the target base, not just that prerequisite issues are closed. If a matching PR exists, reconcile it; update that same PR when authorized.

Assess atomicity. Return NEEDS_SPLIT with a dependency-ordered proposal for multiple independent outcomes. Return ALREADY_IMPLEMENTED with proof if the behavior is already present. Otherwise implement the smallest correct change and focused tests using repository-native conventions; inspect relevant AGENTS.md. Run the focused checks, then required broader checks dictated by risk/project policy. Record exact commands, revision, and pass/fail/skip/unavailable. Do not claim a pass without observed output.

Create or update one focused PR only after checks. Link the issue and summarize scope/evidence. Record evidence in issue comment or linked evidence record. Report PR_READY and stop; do not merge, close, verify as a separate mode, or start another issue. Goal mode is optional for bounded iterative work; its completion target is PR_READY.
