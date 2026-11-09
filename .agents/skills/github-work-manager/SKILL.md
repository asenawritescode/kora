---
name: github-work-manager
description: "Plan atomic GitHub issues and manage one issue through implementation, verification, review, reconciliation, and evidence-based closure. Use when working from a GitHub issue/spec with gh; do not use for untracked broad implementation without selecting a mode."
---

# GitHub Atomic Work Manager

Invocation: `PLAN <repo> <spec-or-issue>`, `NEXT <repo-or-project>`, `EXECUTE <owner/repo#n>`, `VERIFY <issue> <PR>`, `REVIEW <issue-or-PR> <feedback>`, `RECONCILE <repo-or-epic>`, or `CLOSE <issue>`.

Route only to the requested mode. Never chain modes or move to another issue without a fresh explicit request. PLAN and read-only modes never edit code. Planning creates a draft first; obtain approval before GitHub mutation. EXECUTE requires approval, an exact issue, and a clean/safe worktree; complete one slice and stop at PR_READY. CLOSE requires verified merge evidence.

## Invariants

- One active coding issue per repository; global WIP defaults to one. One repo, atomic slice, and focused PR per execution.
- Never implement an epic, linked issue, or dependency automatically. Never silently switch issues or abandon an existing PR.
- Treat unknown dependency, CI, branch, or test metadata as UNKNOWN. Missing evidence is not success.
- GitHub is the task record. PR creation is not completion. DONE requires integration, passing required checks, criterion-level evidence, and issue closure.
- Never discard user changes or modify code during PLAN, VERIFY, RECONCILE, or CLOSE.

## Mode routing

Load only the relevant reference: PLAN → [planning](references/planning.md), NEXT → [GitHub lifecycle](references/github-lifecycle.md) and [model routing](references/model-routing.md), EXECUTE → [execution](references/execution.md), VERIFY/CLOSE → [verification](references/verification.md) and [definition of done](references/definition-of-done.md), REVIEW → [review](references/review.md), RECONCILE → [GitHub lifecycle](references/github-lifecycle.md). Use [issue template](references/issue-template.md) when drafting executable issues.

Use the scripts as deterministic helpers, not as substitutes for GitHub source evidence: `preflight.py`, `issue-status.py`, `evidence.py`. Their input/output contracts are documented in their `--help` and reference files. Use `gh` by default; optional API/MCP is acceptable. If dependency relationships cannot be read, report UNKNOWN and make no selection.

## Result contract

Return the requested mode's compact format from its reference. Use only: BACKLOG, BLOCKED, READY, IN_PROGRESS, PR_READY, IN_REVIEW, CHANGES_REQUESTED, MERGED_PENDING_VERIFY, DONE, NEEDS_SPLIT, NEEDS_REPLAN, ALREADY_IMPLEMENTED, FAILED_VERIFICATION, or UNVERIFIED as applicable. State evidence and blockers. Never claim observed results that were not observed.
