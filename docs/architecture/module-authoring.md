# Module authoring

Modules declare their business resources and the capabilities they provide or
require in `contract.ModuleDescriptor`. Each resource has one owner module.
Package activation validates the declarations and persists the resulting
tenant module snapshot before publishing the new runtime generation.

## Providers and bindings

Register providers through the existing Engine module provider registry. A
provider advertises its owner, internal or external mode, capability contracts,
and operation factories. Tenant configuration selects a provider for each
consumer, capability, and business scope. Mandatory missing or mismatched
providers block activation; optional missing capabilities are reported as
unavailable. A health failure does not select a different provider for work
already pinned to a binding revision.

## Commands, events, and workflows

Mutations go through the declared resource owner's command path, which applies
the caller's actor permissions, module grant, ownership checks, schema,
preconditions, idempotency, and audit. Publish owner facts through the
transactional outbox and consume them through the registered transactional
consumer. Cross-module workflow actions use the pinned capability operation
and its durable recovery state; do not call another module's private tables.

## Operator inspection and recovery

Management clients can read `/api/management/v2/tenants/{id}/modules/inspection`
for module installations, resource owners, selected bindings, capability
availability, blockers, and bounded pending-work counts. The execution route
`/api/management/v2/tenants/{id}/executions/{execution}` reports the pinned
provider and contract plus delivery and reconciliation state. These projections
omit credentials, idempotency material, request payloads, and personal snapshots.
An `outcome_unknown` execution requires owner reconciliation before retry.

CI checks the application-domain import boundary with an intentionally invalid
fixture and exercises module composition modes using fresh programmatic
fixtures. Disposable database suites cover supported persistence dialects.
