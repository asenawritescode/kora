# Kora Organizational Runtime implementation tracker

Branch: `feature/organizational-runtime`

Tasks are executed in this order. A task is marked complete only after its
own tests or validation gate passes.

## Audit invariant — domain packages stay declarative

Inventory/procurement is a reference package, not a runtime mode. Its current
definitions live under `config/inventory/` as YAML. The engine, generic
runtime, and API must not import an inventory package or expose inventory
routes. Package behavior enters through generic resource loaders and the shared
command executor; package-specific YAML is validated as data. The current
engine audit passes this boundary: production Go has no inventory import or
inventory API route. Inventory names that remain in engine tests are fixtures
for proving package isolation and generic loading.

## Phase 0 — Preserve and contract

- [x] ORG-FOUND-001 Preserve and partition repository work.
- [x] ORG-FOUND-002 Establish canonical organizational vocabulary across repositories.
- [x] ORG-FOUND-003 Publish and consume versioned cross-repository fixtures (byte-identical v1 fixture is validated by engine, Cloud, Studio, and website CI gates).
- [x] ORG-FOUND-004 Build the isolated end-to-end acceptance harness (cross-repository gate and disposable live stack verified website onboarding, Cloud provisioning/model activation/package metadata, YAML package import, generic engine registry/command execution, durable outbox/audit events, and Studio production-preview login/workspace rendering; Chrome DevTools MCP was unavailable in this session, so the equivalent installed headless Chrome fallback was used).

### Phase 0 live evidence

The isolated run used disposable MySQL and local services on ports 3308/8000/8081/5173/5175. The website created a fresh tenant and followed the returned workspace URL; Cloud provisioning completed and a model proposal transitioned through validation, approval, and activation; the inventory/procurement package was imported from YAML; the engine exposed only generic command resources and executed `inventory.add_stock` through the shared executor; and the database contained the completed command audit plus pending semantic outbox events. The Studio production preview then authenticated against the same tenant and rendered the workspace with the YAML-provided Inventory module, with no console errors or failed requests.

## Phase 1 — Engine foundations

- [x] KOR-ORG-001 Extend the resource registry for organizational primitives.
- [x] KOR-ORG-002 Add organizational model revisions.
- [x] KOR-ORG-003 Implement capability and skill contracts.
- [x] KOR-ORG-004 Implement policies, roles, actors, and delegation.
- [x] KOR-ORG-005 Build the shared execution runtime.

### Phase 1 completion audit

Phase 1 was re-audited after the Phase 0 live acceptance run. The focused
foundation gate passes for `graph`, `org`, `contract`, `kernel`, and `tests`.
The registry covers all organizational resource kinds with namespaced,
versioned, hashed identities; revisions support preview, activation, and
rollback; capabilities are executor-neutral contracts; authorization fails
closed for ungranted actors and supports expiry/prohibition and approval
gates; and the shared runtime emits the same event/provenance shape for
authorized executions. No domain package imports or domain-specific runtime
routes were introduced.

## Phase 2 — Extensibility and explainability

- [x] KOR-ORG-006 Add scoped capability seams and plugin services (contract/plugin foundation).
- [x] KOR-ORG-007 Add dependency, coeffect, and effect reconciliation (existing graph/reconcile/effect foundation).
- [x] KOR-ORG-008 Add durable semantic event envelopes (validated provider/outbox boundary).
- [x] KOR-ORG-009 Add provenance and explainability records (contract and execution event links).
- [x] KOR-ORG-010 Add guarded agent execution (deny-by-default manifest contract).
- [x] KOR-ORG-011 Add profiles, bundles, patches, and package lifecycle (engine package state machine).

### Phase 2 completion audit

The focused Phase 2 gate passes for `plugin`, `recon`, `reconcile`, `outbox`,
`org`, and `contract`. Plugin providers are accessed through typed capability
handles and scoped service bindings with cleanup; dependency reconciliation
has deterministic drift, lease fencing, and bounded retry behavior; semantic
events use validated durable envelopes and replayable outbox delivery;
provenance records link outputs to actors, capabilities, and events; agent
manifests deny capabilities by default and separate approval-required actions;
and package/profile/patch lifecycle transitions reject unsafe changes while
preserving cleanup and rollback state. All behavior remains provider- and
domain-neutral.

## Phase 3 — Cloud lifecycle

- [x] CLOUD-ORG-001 Establish the control-plane baseline.
- [x] CLOUD-ORG-002 Extend onboarding to organization activation.
- [x] CLOUD-ORG-003 Add model proposal and approval APIs.
- [x] CLOUD-ORG-004 Add package/plugin registry metadata.
- [x] CLOUD-ORG-005 Implement tenant package lifecycle.
- [x] CLOUD-ORG-007 Extend provisioning/deployment jobs.

### Phase 3 completion audit

The Cloud control plane now owns tenant lifecycle, provisioning metadata,
model proposals, package discovery metadata, tenant-scoped package state, and
resumable deployment checkpoints while excluding tenant business records and
runtime command execution. Onboarding captures organization/workflow intent;
proposal APIs support validation, approval, activation, rejection, and
rollback; package metadata includes compatibility, permissions, required
services, migrations, cleanup, security, pricing, licensing, and review state;
tenant package transitions are guarded and reversible; and provisioning jobs
persist step status, health, and recovery information. The full Cloud gate
passes with `go test ./...`, including the control-plane, API, package, model
proposal, and provisioning tests.

## Phase 4 — Studio workspace

- [x] STUDIO-ORG-001 Establish the workspace shell.
- [x] STUDIO-ORG-002 Build onboarding model review.
- [x] STUDIO-ORG-003 Add the Today operational home.
- [x] STUDIO-ORG-004 Implement generic record forms/lists.
- [x] STUDIO-ORG-005 Build the workflow editor.
- [x] STUDIO-ORG-006 Build graph inspection (live entity/workflow relationships with operational links).
- [x] STUDIO-ORG-007 Build provenance/why panels (live redacted operation-audit projection with actor, command, status, correlation, and hash evidence).

### Phase 4 completion audit

Studio’s workspace shell, model review, Today home, generic records/forms,
workflow editor, graph inspector, and provenance surfaces are verified. The
graph route reads active DocTypes and workflows through generic system APIs,
allows progressive resource selection, shows declared operating and policy
relationships, and links to normal operational routes. The engine now exposes
a read-only `/api/v1/system/audit` projection that excludes command arguments
and business payloads; Studio uses it to show recent provenance for selected
entities. Engine `go test ./...`, Studio’s 35 test files/161 tests, and the
Studio production build all pass.

## Phase 5 — Reference package

- [x] KOR-ORG-013 Build the inventory/procurement package (complete declarative package manifest, entities, commands, permissions, rules, and workflow definitions; validated through generic loaders).
- [x] KOR-ORG-014 Add generic integration adapters (existing provider-neutral webhook/email boundaries).
- [x] STUDIO-ORG-008 Build inventory/procurement workspace (reference workspace and YAML view manifest are present and tested with the generic Studio build).
- [ ] STUDIO-ORG-009 Implement stock movement workflows.
- [ ] STUDIO-ORG-010 Implement low-stock procurement workflow.

## Phase 6 — Marketplace and agents

- [ ] CLOUD-ORG-006 Add marketplace publishing.
- [ ] STUDIO-ORG-011 Build agent/delegation screens.
- [ ] STUDIO-ORG-012 Build marketplace/extension manager.
- [ ] STUDIO-ORG-013 Add conversational UI crystallization.

## Phase 7 — Website and launch validation

- [ ] WEB-ORG-001 Replace positioning/information architecture.
- [ ] WEB-ORG-002 Rewrite homepage.
- [ ] WEB-ORG-003 Create How Kora works page.
- [ ] WEB-ORG-004 Create inventory reference page.
- [ ] WEB-ORG-005 Create extensions/marketplace page.
- [ ] WEB-ORG-006 Rewrite onboarding copy/flow.
- [ ] WEB-ORG-007 Align Cloud/open-source/developer messaging.
- [ ] WEB-ORG-008 Add marketing funnel events.
- [ ] WEB-ORG-009 Run copy/accessibility audit.
- [ ] WEB-ORG-010 Purge outdated website framing.
- [ ] CLOUD-ORG-008 Add Cloud observability.
- [ ] CLOUD-ORG-009 Add Cloud-to-engine contract tests.
- [ ] CLOUD-ORG-010 Purge Cloud responsibilities belonging in engine.
- [ ] STUDIO-ORG-014 Add responsive/accessibility/usability validation.
- [ ] STUDIO-ORG-015 Purge terminal builder assumptions.
- [ ] KOR-ORG-015 Purge engine-level domain assumptions.
