# Architecture documentation

The [target-state specification](target-state-spec.md) is the implementation authority for the 2026-10-13 restructuring. It defines packages, ownership, Go and TypeScript contracts, request/approval and workflow semantics, consumer migrations, tests, implementation groups, and deployment gates.

This is a target, not a claim that the implementation already has these properties. Complete and verify one implementation group at a time in dependency order, recording its gate before starting the next group. Release capabilities only after their specified tests pass.

Current group status, validation evidence, and remaining external gates are tracked in [implementation-status.md](implementation-status.md).

## Repository boundaries

- Engine owns tenant business state and execution.
- Cloud owns deployment intent, commercial state, placement and management operations.
- Standalone and embedded UIs consume versioned contracts; they do not implement authorization or business transitions.
- SDKs implement transport and validation, not domain policy.

The standalone UI repository is `kora-studio-mvp`, with package name `kora-ui`. The TypeScript SDK repository is `kora-ts-sdk`, with package name `@kora/extension-sdk`.

## Reading order

1. Specification sections 1–4: decisions, source changes, domain ownership, project structure.
2. Sections 5–7: contract types, resource lifetimes, database and memory limits.
3. Sections 8–10: requests, approvals, durable workflow, persistence and authorization.
4. Section 11: public endpoints and cross-repository migration.
5. Sections 12–15: ordered work, tests, release and removal gates.

## Evidence and operations

Dated audits, UAT results, incident reports and completion records remain evidence of their stated period. They do not override the target specification. `dialect-authoring-guide.md`, product/brand documentation and user guides remain separate references; implementation changes must update affected operational instructions in the same change set.

See [documentation changes](documentation-changes.md) for the documents consolidated in this pass and the preserved historical evidence. The architecture graph in this spec is a target ownership graph; G0 generates the exact source import graph.

Track implementation evidence and release gates in the [implementation status ledger](implementation-status.md).
The [G0 source inventory](g0-inventory.md), [registered route inventory](route-inventory.json), and [contract fixture manifest](contract-fixture-manifest.json) record the current consumer and persistence evidence. `make check-contracts` verifies Engine-local generation and UI drift; `make check-cross-repo-contracts` verifies fixture/SDK copies, registered routes, and the complete Go import graph across the sibling repositories.
Inbound integration release procedures are in [provider callback authentication](provider-callbacks.md).
The current single-engine Compose example and backup/restore procedures are in [`deploy/selfhost`](../../deploy/selfhost/README.md).
Run `make check-selfhost-backup` to verify the source-sequence manifest utility used by the backup and restore procedures.

The current process-level SQL connection admission and its operator allocation
settings are documented in [resource budgets](resource-budgets.md). These
settings divide a declared physical-server connection allowance among Engine
replicas; they do not coordinate replicas dynamically.

The implemented backend and real-database test evidence, including explicit
coverage gaps, is tracked in the [database support matrix](database-support-matrix.md).
The current Engine worker, lifecycle, and SQL-admission boundaries are inventoried
in [worker admission inventory](worker-admission-inventory.md).
