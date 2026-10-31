# Documentation reconciliation — 2026-10-13

This change publishes the target specification and reconciles documentation. Application implementation, database migrations, deployments and tests specified in it remain scheduled work.

## Engine

Created `docs/architecture/target-state-spec.md`, this change record and the architecture index. Added narrow Git ignore exceptions so these three documents can be reviewed and committed. Other private documentation remains private.

Updated README and contributor-facing CLAUDE guidance with the target entry point while retaining existing instructions and working-tree edits. Replaced `FRONTEND-ADAPTATION-PLAN.md` with the current consumer migration, retaining manifest rendering, capability discovery, realtime invalidation and separation of server/shell state.

Removed these superseded active plans:

| Removed file | Replacement |
|---|---|
| `docs/backend-architecture.md` | Specification sections 2–7 |
| `docs/kora-cloud-architecture.md` | Specification section 11 and Cloud repository architecture |
| `docs/site-directory-architecture-plan.md` | Specification sections 11.5 and 14; dated verification extracted separately |
| `docs/extensibility-plan.md` | Domain action and bounded script contracts in sections 7 and 9 |
| `docs/extensibility-process-isolation.md` | Explicit embedded-runtime limits and deferred process isolation in section 7 |
| `docs/architecture-migration-libsql.md` | Dialect-aware migration/recovery rules and real transport tests in sections 10 and 13 |
| `docs/saas-architecture-gap-analysis.md` | Current ownership and self-hosted/deployment plan in sections 3, 11 and 14 |

Historical phase completion records and security/plugin/Cloud reviews are explicitly labelled as dated evidence. Their old follow-up phases do not control new implementation. Site-directory verification was extracted to `docs/site-directory-verification-20261013.md`; previous local timing/count results are not current capacity guarantees.

## Cloud

Replaced obsolete deployment prescriptions in `docs/kora-cloud-architecture.md` with a management-client boundary and simple deployment pools. Preserved dated commercial material in `docs/history/cloud-commercial-planning-20260630.md` and the earlier ticket planning source in `docs/history/architecture-specification-before-20261013.md`.

Updated `docs/cloud-boundary.md`, ticket execution specification/indexes and request/approval PRD ownership. Existing product requirements and unrelated source edits are preserved. The old ticket collection requires per-ticket reconciliation against G0–G10 before execution; it is not a competing target. Cloud business operation clients and managed-channel credentials remain separate from management provisioning credentials.

## Standalone UI and SDK

Added `docs/architecture-contracts.md` in each repository and README links. Planned source changes include generated models/decoders, typed request/workflow calls, inbox routes, compatibility tests and later deletion of duplicate transport DTOs. No UI/SDK source was changed in this documentation pass.

## Preservation and remaining scope

Original affected documentation was copied before reconciliation to `/home/asena/Downloads/kora-architecture-docs-before-20261013/`, with a SHA-256 manifest. This external backup preserves working-tree material removed from active architecture docs. Application code, configuration, data and unrelated edits were not reset or changed.

This is not a rewrite of all product PRDs, old ticket bodies, incident reports or audit evidence. Those retain their historical context; the active architecture entry points now select one target. G0 inventories remaining references during implementation and G10 removes obsolete implementation-specific instructions alongside their replaced code.
