# VERIFY and CLOSE

## VERIFY
Read exact issue ACs, associated PR and diff, changed-file list, head/base and merge state, required checks and CI runs, contracts, and review threads. Do not edit production code. For each criterion report PASS/FAIL/UNVERIFIED with evidence (test/CI URL, diff location, or inspection result). Distinguish passed, failed, skipped, and unavailable checks. Check unrelated scope and integration evidence. A PR that is open is not merged; missing CI or relationship metadata is UNKNOWN. Do not merge.

Output a compact AC matrix, CI/review findings, and verdict: verified, FAILED_VERIFICATION, or UNVERIFIED. Verification applies to the observed revision only.

## CLOSE
Confirm merged into the required integration branch; identify merge commit and expected changes in it. Confirm mandatory ACs and post-merge checks on applicable revision; no required silent skips or unresolved blocking review threads. Record evidence on issue, then close only after every gate passes. On a missing required check use UNVERIFIED and leave open unless an authorized waiver explicitly documents rationale and residual risk. Re-evaluate direct dependents for readiness; do not execute them. Report merge SHA, final checks, evidence URL, closure state, newly unblocked issues.
