# Kora Organizational Runtime implementation tracker

Branch: `feature/organizational-runtime`

Tasks are executed in this order. A task is marked complete only after its
own tests or validation gate passes.

## Phase 0 — Preserve and contract

- [x] ORG-FOUND-001 Preserve and partition repository work.
- [ ] ORG-FOUND-002 Establish canonical organizational vocabulary across repositories.
- [ ] ORG-FOUND-003 Publish and consume versioned cross-repository fixtures.
- [ ] ORG-FOUND-004 Build the isolated end-to-end acceptance harness.

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

- [ ] STUDIO-ORG-001 Establish the workspace shell.
- [ ] STUDIO-ORG-002 Build onboarding model review.
- [ ] STUDIO-ORG-003 Add the Today operational home.
- [ ] STUDIO-ORG-004 Implement generic record forms/lists.
- [ ] STUDIO-ORG-005 Build the workflow editor.
- [ ] STUDIO-ORG-006 Build graph inspection.
- [ ] STUDIO-ORG-007 Build provenance/why panels.

## Phase 5 — Reference package

- [ ] KOR-ORG-013 Build the inventory/procurement package.
- [ ] KOR-ORG-014 Add generic integration adapters.
- [ ] STUDIO-ORG-008 Build inventory/procurement workspace.
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
