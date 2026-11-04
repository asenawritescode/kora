# Backend modules

## Authoring

A package may declare one or more modules. Each module has a stable namespaced ID and release version. Declare every owned resource with a `ResourceRef`, access level, and mutation mode. `commands_only` resources reject generic record writes; publish explicit capability operations for domain changes. Capability contracts use exact registered `SchemaRef` versions.

Modules declare required capabilities as required or optional. Provider registrations identify their owner, tenant scope, internal/external kind, offers, and runtime factory. Tenant startup resolves all requirements deterministically. Ambiguous providers require an explicit selection; required missing or unavailable providers block publication. A published runtime generation retains its binding plan.

## Data and references

Use ordinary local `Link` fields for local row relationships. Use provider-qualified `BusinessRef` identities for public cross-module references, and capture an approved `HistoricalSnapshot` when an issued document must remain readable without its source provider.

## Commands, events, and workflows

Only trusted dispatch can create `ModuleExecution` provenance. Domain commands check actor authorization and owner identity, then write through the kernel transaction. Business events name the owning module and payload schema and append through the existing transactional outbox. Cross-module external actions pin provider, binding revision, schema, idempotency key, and input hash before the first effect attempt. Unknown outcomes require reconciliation before retry.

## Failure recovery

Config activation journals DDL plans and commits module ownership with the active config snapshot. Retries use the stored artifact identity and activation revision. External effects remain pinned across provider changes; confirmed non-application may retry with the same idempotency identity, while unknown outcomes are blocked pending reconciliation. Module disable follows installed → active → draining → disabled; removal requires no dependent modules or unresolved owned work and never deletes business history.
