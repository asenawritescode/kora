# Kigae Engine — Phase 0 Contract Extraction (Evidence Record)

Status: historical review/completion evidence. Statements and follow-up phases describe the original review period. Use [the target specification](architecture/target-state-spec.md) for new implementation work. Superseded source plans were removed from the active documentation.

This document records the completion of **Phase 0 — contract extraction** from
`KORA-ENGINE-RFC.md`. It is the evidence record required by the RFC handoff rule
(§19–§21): each step must deliver code, tests, and a short evidence record, and a
phase is not complete until its contracts are frozen and tested.

## Scope

Phase 0 extracts the provider-neutral, versioned wire contracts from RFC §7,
§7.1, §7.3, and §8 into a single canonical package (`contract/`) that every
adapter (HTTP, chat, channel, MCP, SDK, UI, NATS) projects from. It does **not**
yet build the outbox, NATS provider, actors, offline sync, or cloud planes —
those are Phases 1–6.

## Deliverables

| Artifact | Location | Purpose |
|----------|----------|---------|
| Envelope & error contracts | `contract/contract.go` | `EventEnvelope`, `CommandEnvelope`, `ActorContext`, `CommandResult`, `TaskReceipt`, `Delivery`, `Error`, stable `Status` and `Code` values |
| Tool catalog contracts | `contract/tool.go` | `ToolDescriptor`, `ToolCatalog`, `FieldHint`, `SystemFieldHint`, `UsageEvent`, `Approval`, `Cursor` |
| ID & version helpers | `contract/id.go` | ULID generation, `CurrentVersion`, `EncodeData` |
| Capability registry | `contract/capability.go` | Status vocabulary (`planned`/`experimental`/`supported`/`retired`), blocking-risk gate |
| Contract version tests | `contract/contract_test.go`, `contract/tool_test.go`, `contract/capability_test.go` | JSON round-trip, stable codes, deadline semantics, identity fail-closed, capability gates |
| Tool catalog parity bridge | `api/ai/tool_contract.go`, `api/ai/tool_contract_test.go` | `ToContractDescriptor`/`ToContractCatalog` project `BuildToolCatalog` into the canonical wire shape |

## Frozen contracts

The following wire contracts are frozen at `contract.CurrentVersion = 1`. Their
JSON shape is the source of wire compatibility; breaking changes require a
version bump.

- **Status** — `completed`, `accepted`, `rejected`, `conflict`, `failed`, `pending`.
- **Error codes** — `PERMISSION_DENIED`, `VALIDATION_FAILED`, `NOT_FOUND`,
  `CONFLICT`, `DEADLINE_EXCEEDED`, `DEPENDENCY_UNAVAILABLE`,
  `IDEMPOTENCY_KEY_REUSED`, `UNAUTHENTICATED`, `INTERNAL_ERROR`.
- **PrincipalType** — `human`, `service`, `agent`.
- **Projection** — `metadata`, `changed_fields`, `summary`, `full_document`.

## Capability status

The canonical capability registry (`contract.BaselineCapabilities()`) records
the current status of each capability. It is the source of truth for public
capability docs and should stay aligned with implementation reality.

| Capability | Status | Blocking risks |
|------------|--------|----------------|
| `contract.event_envelope` | `supported` | — |
| `contract.command_envelope` | `supported` | — |
| `contract.actor_context` | `experimental` | identity, authorization |
| `provider.nats` | `supported` | — |
| `outbox.transactional` | `supported` | — |
| `auth.oidc` | `supported` | — |
| `ai.chat` | `experimental` | authorization, durable-business-effects |
| `ai.mcp` | `experimental` | authorization, credential-scope |
| `workflow.actor` | `supported` | — |
| `offline.sync` | `supported` | — |

Current supported capability set:

- `contract.event_envelope`
- `contract.command_envelope`
- `provider.nats`
- `outbox.transactional`
- `auth.oidc`
- `workflow.actor`
- `offline.sync`

Current experimental capability set:

- `contract.actor_context`
- `ai.chat`
- `ai.mcp`

## Tests

Contract version tests live in `contract/*_test.go` and cover:

- Stable machine-readable `Status`, `Code`, `PrincipalType`, and `Projection`
  values.
- JSON round-trips and frozen field-name presence for command, event, tool,
  usage, approval, and cursor contracts.
- Deadline semantics (missing/expired deadlines fail before business execution).
- Fail-closed identity (`ActorContext.Authenticated()`).
- Capability registration, unknown-name fail-closed reads, blocking-risk gating,
  duplicate-registration panic.

## Follow-up phases (not started)

- **Phase 1** — `_kora_outbox` + consumer receipts; move analytics/webhooks/hooks
  behind worker interfaces; LocalProvider/NATS parity.
- **Phase 1A** — authentication foundation (provider registry, OIDC+PKCE).
- **Phase 2** — NATS provider (`nats.go`).
- **Phase 3 / 3A** — workflow actors, AI safety & metering.
- **Phase 4** — standalone composable UI (`kora-ui`).
- **Phase 5** — offline vertical slice.
- **Phase 6** — Kigae Cloud control/data planes (`kora-cloud`).
