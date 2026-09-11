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
- [ ] ORG-FOUND-004 Build the isolated end-to-end acceptance harness (cross-repository local gate now exists at `scripts/organizational-runtime-acceptance.sh`; live website→Cloud→engine→Studio business-flow execution remains).

## Phase 1 — Engine foundations

- [x] KOR-ORG-001 Extend the resource registry for organizational primitives.
- [x] KOR-ORG-002 Add organizational model revisions.
- [x] KOR-ORG-003 Implement capability and skill contracts.
- [x] KOR-ORG-004 Implement policies, roles, actors, and delegation.
- [x] KOR-ORG-005 Build the shared execution runtime.

## Phase 2 — Extensibility and explainability

- [x] KOR-ORG-006 Add scoped capability seams and plugin services (contract/plugin foundation).
- [x] KOR-ORG-007 Add dependency, coeffect, and effect reconciliation (existing graph/reconcile/effect foundation).
- [x] KOR-ORG-008 Add durable semantic event envelopes (validated provider/outbox boundary).
- [x] KOR-ORG-009 Add provenance and explainability records (contract and execution event links).
- [x] KOR-ORG-010 Add guarded agent execution (deny-by-default manifest contract).
- [x] KOR-ORG-011 Add profiles, bundles, patches, and package lifecycle (engine package state machine).

## Phase 3 — Cloud lifecycle

- [ ] CLOUD-ORG-001 Establish the control-plane baseline.
- [ ] CLOUD-ORG-002 Extend onboarding to organization activation.
- [ ] CLOUD-ORG-003 Add model proposal and approval APIs.
- [ ] CLOUD-ORG-004 Add package/plugin registry metadata.
- [ ] CLOUD-ORG-005 Implement tenant package lifecycle.
- [ ] CLOUD-ORG-007 Extend provisioning/deployment jobs.

## Phase 4 — Studio workspace

- [x] STUDIO-ORG-001 Establish the workspace shell.
- [x] STUDIO-ORG-002 Build onboarding model review.
- [x] STUDIO-ORG-003 Add the Today operational home.
- [x] STUDIO-ORG-004 Implement generic record forms/lists.
- [x] STUDIO-ORG-005 Build the workflow editor.
- [ ] STUDIO-ORG-006 Build graph inspection (generic shell committed; live resource/provenance data remains).
- [ ] STUDIO-ORG-007 Build provenance/why panels (generic shell committed; live source links remain).

## Phase 5 — Reference package

- [ ] KOR-ORG-013 Build the inventory/procurement package (YAML package definitions are present; generic command execution and workflow verification remain).
- [x] KOR-ORG-014 Add generic integration adapters (existing provider-neutral webhook/email boundaries).
- [ ] STUDIO-ORG-008 Build inventory/procurement workspace (reference UI committed; YAML-driven generic rendering remains).
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
