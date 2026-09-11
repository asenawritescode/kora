# Organizational Runtime Change Ledger

Coordinated branch: `feature/organizational-runtime`

This ledger records the first implementation slice of the approved
organizational-runtime backlog. Existing work was preserved before branching.

| Ticket | Owner | Status | Scope |
|---|---|---|---|
| ORG-FOUND-001 | all repositories | in progress | Branches created; current dirty states preserved. Studio directory is present but is not a Git checkout. |
| ORG-FOUND-002 | engine / Cloud / Studio | implemented (engine slice) | Canonical organizational resource kinds and lifecycle added to the engine contract. |
| ORG-FOUND-003 | all repositories | started | Versioned engine fixture added; downstream repositories should consume the same shape. |
| KOR-ORG-001 | Kora engine | implemented (memory slice) | Registry accepts organizational kinds, validates unknown kinds, and retains version/hash identity. |
| KOR-ORG-002 | Kora engine | implemented (memory slice) | Model revisions support preview, activation, and rollback. |
| KOR-ORG-003 | Kora engine | implemented (contract slice) | Capability and skill contracts added; shared executor added. |
| KOR-ORG-004 | Kora engine | implemented (contract slice) | Explicit actor grants and fail-closed authorization added. |
| KOR-ORG-005 | Kora engine | implemented (memory slice) | Human/agent/integration-neutral execution path emits semantic events. |
| KOR-ORG-008 | Kora engine | implemented (validation slice) | Event envelopes validate before durable outbox append. |

The remaining Cloud, Studio, website, SQL persistence, package distribution,
marketplace, and end-to-end acceptance work remains queued behind these
contracts. No existing `kora-cloud` changes were reset, deleted, or amended.
