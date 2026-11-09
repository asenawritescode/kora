# Definition of done

DONE requires all of the following, with evidence on the issue or linked ledger:

1. Every mandatory AC has criterion-level evidence.
2. Change is merged into the required integration branch and merge commit is identified.
3. Required tests and CI pass on the applicable revision; required integration checks ran.
4. No required test was silently skipped; unavailable checks are UNVERIFIED.
5. No blocking review thread remains.
6. No unauthorized scope expansion occurred.
7. Evidence is recorded, then issue is closed.

Accepted exception requires explicit authorized waiver, rationale, and residual risk; record it. PR_READY is not DONE. An open PR is not DONE. A closed prerequisite without integrated code is not evidence. Evidence ledger fields: issue URL/ID; base revision and integration branch; PR URL/head; merge SHA; AC identifiers; exact commands/results; CI URLs; contract revision/digest if relevant; manual checks; review blockers; disposition. Keep logs linked, not copied wholesale.
