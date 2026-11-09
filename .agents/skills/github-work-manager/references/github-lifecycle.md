# GitHub lifecycle and selection

GitHub is authoritative for issue, PR, dependency, and check state. Use `gh` with the exact repository. Labels/project fields may map lifecycle states; document the configured mapping and do not pretend states are native. Avoid mutating state in read-only modes.

NEXT algorithm: fetch open candidates; exclude epics/planning/non-executable items; unknown or unfinished blockers; blockers whose implementation is not integrated; active implementation PRs; insufficient ACs; repo with active execution; and full global WIP. Rank eligible tasks by configured priority, then count of directly blocked eligible work they unlock, then oldest creation time. Select exactly one. Issue number is never execution order. If GitHub dependency metadata is unavailable, return no selection/UNKNOWN, not assumed-ready. Closed blockers alone do not prove integration.

WIP default is one globally and one per repository. Read configured limits if provided; never silently raise them. Selection is at most one, including when a multi-repo project is queried.

State mapping: BACKLOG, BLOCKED, READY, IN_PROGRESS, PR_READY, IN_REVIEW, CHANGES_REQUESTED, MERGED_PENDING_VERIFY, DONE. Derive from observable records and evidence; do not claim a label is present unless queried. `issue-status.py` validates a supplied snapshot but does not call GitHub.

RECONCILE: audit open issues/PRs/branches/worktrees, merge commits, review threads, and dependency state; identify duplicate/superseded/completed-unmerged/blocked/missing work. Recommend smallest safe order. No code or GitHub mutations.
