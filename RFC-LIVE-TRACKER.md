# Kora Engine RFC Live Tracker

This file is the working tracker for `KORA-ENGINE-RFC.md`.

- Status legend:
  - `[ ]` not started
  - `[-]` in progress
  - `[x]` done
  - `[!]` blocked or needs decision
- Update rule:
  - Keep the current phase and the next two dependency layers visible.
  - Add newly discovered edge cases here before closing a round of work.
  - Do not mark a phase complete until the RFC acceptance gates and tests are attached.

## Current Focus

- [-] Phase 0: contract extraction
- [-] Phase 1: outbox and worker contracts
- [-] Phase 1A: authentication foundation
- [-] Phase 4A: visual builder and live application surface
- [x] Operation kernel first vertical slice (theta architecture backlog KERNEL-001..007): canonical `record.create`/`record.update` commands through one kernel path with tenant isolation, authorization parity, idempotency receipts, optimistic concurrency, atomic audit + outbox commit, and delivery/retry evidence — implemented in the new `kernel/` package with HTTP adapter at `POST /api/v1/kernel/:command` and integration suite in `kernel/kernel_test.go` (9 scenarios, MySQL harness; PostgreSQL reference pending DB-001).

## Branch Reality Check

- The kernel slice is present and materially advanced. The branch still has open RFC gaps in worker backpressure/diagnostics and the remaining Phase 4A builder/runtime parity work.
- Do not treat tracker `[x]` markers as a blanket release claim unless the RFC gate listed in `KORA-ENGINE-RFC.md` has been explicitly re-verified.
- Phase 4A remains the clearest unfinished area: the repository has manifest/runtime support, but builder-as-authoring-mode, semantic layout persistence, live reconnect parity, and browser proof are still not closed.


## Master Task List

### Phase 0: contract extraction

- [x] Freeze `EventEnvelope`, `CommandEnvelope`, and provider-neutral contract types.
- [x] Keep current local WAL behavior behind `LocalProvider`.
- [x] Add event IDs, versions, correlation IDs, and idempotency coverage.
- [x] Replace pointer-based async hook requests with serializable DTOs.
- [x] Mark chat and MCP behavior as experimental until shared authorization and identity gates pass.
- [x] Extract a single `ToolCatalog` from the registry.
- [x] Add provider-profile validation and immutable usage records to the core contract.
- [-] Audit every remaining ad hoc payload shape against the frozen contract types.
- [x] Verify every exported contract has a stability test.
- [x] Compare generated schemas against all runtime projections.

### Phase 1: outbox and worker contracts

- [x] Add `_kora_outbox` storage and consumer receipt groundwork.
- [x] Move analytics, webhooks, and async hooks behind worker interfaces.
- [x] Make local provider delivery semantics match the contract shape used by NATS.
- [x] Stop silently dropping critical async work.
- [x] Add overflow and backpressure metrics for every async queue.
- [x] Add dead-letter visibility for replayable worker failures.
- [x] Prove redelivery does not duplicate business effects.
- [x] Verify recovery behavior after worker restart and process crash.

### Phase 1A: authentication foundation

- [x] Replace hardcoded auth responses with a provider registry.
- [x] Preserve password and magic-link behavior while normalizing session handling.
- [x] Add provider configuration, secret references, identity links, auth attempts, auth events, and account-link records.
- [x] Implement OIDC authorization-code + PKCE first.
- [x] Add issuer, JWKS, redirect, claim, logout, and failure recovery checks.
- [x] Implement shared identity reconciliation and fail-closed actor resolution.
- [x] Add cross-site and cross-tenant auth isolation tests.
- [x] Add callback-replay and key-rotation tests.
- [x] Add session-revocation and deprovisioning tests.
- [x] Verify every auth provider is advertised from enabled configuration only.

### Phase 2: NATS provider

- [x] Add NATS provider scaffolding.
- [x] Provision streams and consumers idempotently.
- [x] Implement request/reply command service behavior.
- [x] Add JetStream publisher/consumer tests with a real local NATS server.
- [x] Add CLI setup and Docker Compose support.
- [x] Validate NATS permissions against tenant/account boundaries.
- [x] Test drain, restart, and duplicate delivery behavior.
- [x] Test fallback mode remains explicit and never silent.
- [x] Add operational diagnostics for stream and consumer health.

### Phase 3: workflow actors

- [x] Define workflow instance state and the SQL-backed timer scheduler.
- [x] Implement actor lease/fencing with KV plus SQL state.
- [x] Migrate one approval workflow end-to-end.
- [x] Add replay, retry, dead-letter, and recovery tests.
- [x] Implement the AI run actor using the same fencing and durable timer contracts.
- [x] Add stale-owner rejection tests under redelivery.
- [x] Add duplicate timer delivery tests with one business effect.
- [x] Add crash/restart recovery tests for in-flight workflow steps.
- [x] Add persistence tests for checkpoint/resume semantics.
- [x] Add step-level audit records for actor transitions.
- [x] Add workflow timeout and cancellation propagation tests.
- [x] Add queue-depth and lease-expiry observability.
- [x] Confirm workflow actors do not create a separate chat state machine.

### Phase 3A: AI safety and metering gate

- [x] Move authorization, confirmation, recent-auth, identity, idempotency, and audit into the shared executor.
- [x] Wire provider deadlines and bounded HTTP transport timeouts.
- [x] Add atomic scoped budget reservations and per-run limits.
- [x] Persist provider-attempt usage events for every call.
- [x] Implement server-side conversations and runs with resume, cancel, checkpoints, and retention.
- [x] Replace adapter-specific tool generators with catalog projections.
- [x] Decide which MCP paths are validation-only versus executable per deployment.
- [x] Add adversarial prompt-injection regression tests.
- [x] Add cycle and stall detection tests.
- [x] Add malformed-provider payload and typed error tests.
- [x] Add duplicate-delivery and provider-failover tests.
- [x] Add audit coverage for every tool call and model attempt.
- [x] Add budget exhaustion and quota rejection tests.
- [x] Add compaction and resume token rotation tests.
- [x] Ensure no secret or sensitive summary leaks into browser payloads.

### Phase 4: standalone composable UI

- [x] Create a standalone `kora-ui` build with runtime configuration injection.
- [x] Define JSON Schemas for page manifests, resources, actions, components, and package metadata.
- [x] Implement the component registry with lazy loading and capability negotiation.
- [x] Convert the dashboard and one module workspace to manifests.
- [x] Route existing CRUD list/new/edit flows through generated compatibility manifests.
- [x] Add manifest validation at publish and load time.
- [x] Add content hashes, ETags, and signature verification.
- [x] Add draft, preview, active, retired lifecycle handling.
- [x] Add rollback support to immutable versions.
- [x] Add one installable frontend package with a page and component.
- [x] Add renderer tests for permissions, unsupported versions, malformed data, loading, error, empty, responsive, and offline states.
- [x] Add accessibility and low-end mobile performance checks.
- [x] Ensure unsupported components fail closed instead of rendering blanks.

### Phase 4A: visual builder and live application surface

- [-] Make the builder an authoring mode of the production renderer, not a separate approximation.
- [x] Persist normalized semantic manifests with deterministic regions, order, nesting, spans, and responsive overrides.
- [ ] Add guided templates, schema-aware palette, semantic component tree, constrained drag/drop, keyboard placement, and property inspector. The guided-template generator and palette contract now have stronger evidence in `ui/src/manifest/runtime/standard-pages.test.ts`, but the interactive builder surface still needs end-to-end proof.
- [x] Add DocType/resource-aware binding and action pickers with inline validation and publish preflight.
- [x] Add exact desktop/tablet/mobile preview, real permission evaluation, representative loading/empty/error/offline states, and source/visual parity.
- [x] Add draft autosave, undo/redo, reload recovery, immutable preview/publish/rollback, and visible save status.
- [ ] Deliver a first-run POS setup that reaches a usable register in five short steps with safe defaults, sample data, resumable progress, and no required technical choices.
- [ ] Add direct POS customization for moving, resizing, hiding, duplicating, removing, and adding cards with visible drop targets, undo, reset-to-default, and predictable portrait/landscape behavior.
- [ ] Connect the POS starter to the backend offline capability and shared sync coordinator, including local snapshots, queued approved operations, cursors, conflicts, revocation, and clear `Ready`, `Syncing`, `Up to date`, and `Needs attention` states.
- [ ] Add authenticated WebSocket transport through the realtime gateway with scoped subscriptions, heartbeat, resume cursor, deduplication, missed-event recovery, and authoritative refetch. Stable event IDs now flow through local WAL/change events and SSE, Studio deduplicates identified events across reconnect/fallback, the gateway replays site-scoped outbox events after `after`/`Last-Event-ID`, both transports filter `scope`/`resource` subscriptions, and replay emits a completion marker with cursor/truncation state that triggers authoritative Studio invalidation; replay-window/refetch browser proof remains open.
- [ ] Add realtime notifications for product/stock/payment/task/sync/operation changes with notification-center history, read/ack commands, severity, related record/action, redaction, and offline-safe display.
- [ ] Audit setup, builder, POS, empty, error, offline, and publish copy for plain language, consistent terms, constructive next steps, and outcome-focused buttons.
- [x] Add typed realtime connection state, invalidation, operation progress, reconnect/resume, and authoritative refetch to live page presets.
- [ ] Add browser tests from builder composition through active route, including invalid manifests, permission changes, reconnect, and rollback. Route-level contract evidence now exists for manifest pass-through, validation failure, and not-found handling in `ui/src/manifest/runtime/ManifestRouteRenderer.test.tsx`; runtime renderer evidence now also covers permission-denied, offline, conflict, and stale states in `ui/src/manifest/runtime/ManifestRenderer.test.tsx`; builder helper evidence now covers bound-component defaults and primary doctype wiring in `ui/src/routes/workspace/admin/page-manifests/editor-builders.ts` and `ui/src/routes/workspace/admin/page-manifests/editor-builders.test.ts`; live Puppeteer evidence now covers authenticated tenant workspace load, dynamic module-first navigation, Settings Marketplace placement, generic module overview rendering, tenant-prefixed realtime proxy success, and zero post-login API/console errors, but builder composition, reconnect, rollback, and full browser proof portions remain open.
- [ ] Keep this phase `planned` until builder/runtime parity and live-page reconnect gates pass.

### Phase 5: offline vertical slice

- [x] Implement device and branch operation logs with schema gates.
- [x] Add tombstone retention and explicit conflict states.
- [x] Implement local apply, central intake, acknowledgement, and conflict records.
- [x] Deliver one POS or warehouse workflow offline-first.
- [x] Add branch sync observability and reconciliation UI.
- [x] Add cursor-repeatable push/pull tests.
- [x] Add stale-write, duplicate-ack, and skipped-schema-version tests.
- [x] Add offline retention and garbage-collection policy tests.
- [x] Verify rejected operations remain retryable or resolvable.

### Phase 6: Cloud control/data planes

- [x] Add NATSDeployment registration and credential references.
- [x] Add tenant accounts, quotas, and stream/KV/Object Store bootstrap.
- [x] Add NATS compatibility, permission, backup, and restore validation.
- [x] Add explicit unreachable, incompatible, and draining states.
- [x] Add managed worker placement and autoscaling.
- [x] Add package registry and deployment rollout.
- [x] Add managed backups, observability, billing, and regional placement.
- [x] Preserve delegated identity across Cloud-to-engine routing.
- [x] Route channels to engine runs instead of duplicating engine logic.
- [x] Prove Cloud provisioning is idempotent and resumable across restart.
- [x] Add deletion, isolation, and RPO/RTO evidence.
- [x] Keep any future Kora-managed NATS behind the same contract.

## Cross-Cutting Work

- [x] Keep capability status in sync with implementation reality.
- [x] Generate public docs from capability status, not aspirational labels.
- [x] Add contract parity tests across chat, MCP, SDK, UI, HTTP, and NATS projections.
- [x] Add end-to-end duplicate-delivery tests under concurrent publishers and restarts.
- [x] Add data-integrity gates for every new phase before moving forward.
- [x] Attach short evidence notes to each phase milestone.
- [-] Keep public APIs free of internal fallback or synthetic privileged-user behavior.
- [ ] Ensure unsupported capabilities stay explicitly marked as such.
- [ ] Maintain a short list of newly discovered edge cases after each implementation round.
- [ ] Re-check RFC acceptance criteria before closing any phase.

## Known Gaps to Track Closely

- [ ] Durable internal task tracker that updates after every round.
- [ ] Dynamic follow-up queue that accumulates new edge cases.
- [ ] Resume path that can continue unfinished work without a new user prompt.
- [ ] Explicit phase-completion evidence bundle for every supported capability.
- [ ] Workflow approval grant should fail closed if run-plan resumption cannot be persisted.
- [ ] Tool catalog parity across all adapters.
- [ ] Shared executor coverage for every tool invocation path.
- [ ] Tenant isolation coverage for SQL, NATS, KV, Object Store, cache, logs, traces, metrics, backups, and credentials.
- [ ] Cloud provisioning recovery after control-plane restart.
- [ ] Offline conflict UI and conflict resolution lifecycle.
- [ ] Final SSE/WebSocket protocol decisions. Stable event identity, bounded client deduplication, site-scoped outbox replay after a cursor, `scope`/`resource` filtering, and replay completion/truncation signaling are implemented and tested; browser recovery proof remains to be specified.
- [ ] Builder/runtime parity evidence across desktop, tablet, and mobile.
- [ ] Deterministic semantic layout contract; no editor-only coordinates or random orientation.
- [ ] Builder publish preflight and draft autosave recovery behavior.
- [ ] Live-page invalidation, operation progress, reconnect, rollback-state, and stale-data UX. Realtime invalidation and notification handoff are now evidenced in `ui/src/lib/realtime.test.ts`, rollback endpoint coverage is now evidenced in `ui/src/lib/api/system.test.ts`, SSE reconnect fallback is now evidenced in `ui/src/lib/realtime.test.ts`, stale resource rendering is now evidenced in `ui/src/manifest/runtime/ManifestRenderer.tsx` and `ui/src/manifest/runtime/ManifestRenderer.test.tsx`, required-field operation progress is now evidenced in `ui/src/components/forms/DocumentForm.tsx`, `ui/src/components/forms/ProgressBar.tsx`, and `ui/src/components/forms/ProgressBar.test.tsx`, rollback dialog copy is now evidenced in `ui/src/routes/workspace/admin/versions-helpers.ts` and `ui/src/routes/workspace/admin/versions-helpers.test.ts`, and root live-header badge/notification copy is now evidenced in `ui/src/components/layout/root-layout-helpers.ts` and `ui/src/components/layout/root-layout-helpers.test.ts`, but the browser-level rollback-state UX and broader builder/browser proof remain open.

## Evidence Notes

- Phase 0: contract extraction, capability registry, and tool-catalog parity tests pass in `contract` and `api/ai`.
- Phase 1: outbox, auth, and worker-interface coverage are verified in the existing package test suites; queue metrics, replay visibility, redelivery dedup, and worker restart/crash recovery are now evidenced by `outbox/recovery_test.go`.
- Phase 1A: provider registry, session isolation, callback/state handling, revocation, and channel/extension auth tests pass; enabled-config advertisement is now explicit, callback replay plus session revocation/deprovisioning coverage is now evidenced in `auth/oidc/oidc_test.go` and `auth/session_test.go`, and cross-site/cross-tenant isolation is now evidenced in `auth/site.go` and `auth/site_test.go`. Key-rotation coverage is now evidenced by the JWKS key-selection regression in `auth/oidc/validate.go` and `auth/oidc/oidc_test.go`.
- Phase 2: NATS provider bootstrap, request/reply, dead-letter, diagnostics, fallback, and tenant/account boundary checks pass in `natsprovider`, `cli`, and `cloud`.
- Phase 3: workflow actor lease, timer recovery, dead-letter, and observability tests pass in `workflow`.
- Phase 3A: shared executor, provider timeout, budget, conversation, and tool governance tests pass in `api/ai`.
- Phase 4: UI manifest and projection tests pass in `ui`.
- Phase 4A: partially implemented; normalized semantic-manifest persistence is now evidenced in `ui/src/manifest/schema/page.test.ts`, guided template generation across the supported presets is evidenced in `ui/src/manifest/runtime/standard-pages.test.ts`, resource/action pickers plus publish preflight are evidenced in `ui/src/routes/workspace/admin/page-manifests/editor.tsx` and `ui/src/routes/workspace/admin/page-manifests/editor-helpers.test.ts`, draft autosave/reload recovery is evidenced in the editor helper tests, exact desktop/tablet/mobile preview parity is now evidenced in `ui/src/routes/workspace/admin/page-manifests/editor.tsx` and `ui/src/routes/workspace/admin/page-manifests/editor-helpers.test.ts`, route-level builder-to-active-manifest pass-through and validation gating are evidenced in `ui/src/manifest/runtime/ManifestRouteRenderer.test.tsx`, runtime renderer permission/offline/conflict/stale states are evidenced in `ui/src/manifest/runtime/ManifestRenderer.tsx` and `ui/src/manifest/runtime/ManifestRenderer.test.tsx`, typed realtime live-page invalidation plus notification handoff is evidenced in `ui/src/lib/realtime.ts`, `ui/src/lib/realtime.test.ts`, and `ui/src/routes/workspace/$doctype/$name.tsx`, stable event identity is evidenced in `analytics/event.go`, `analytics/bus.go`, `api/system.go`, and `analytics`/`api` tests, bounded client deduplication is evidenced in `ui/src/lib/realtime.ts` and `ui/src/lib/realtime.test.ts`, site-scoped outbox replay after `after`/`Last-Event-ID` is evidenced in `api/system.go` and `api/system_realtime_test.go`, client cursor forwarding is evidenced in `ui/src/lib/realtime.ts`, rollback endpoint evidence is now present in `ui/src/lib/api/system.test.ts`, SSE reconnect fallback is now evidenced in `ui/src/lib/realtime.test.ts`, required-field operation progress is now evidenced in `ui/src/components/forms/DocumentForm.tsx`, `ui/src/components/forms/ProgressBar.tsx`, and `ui/src/components/forms/ProgressBar.test.tsx`, rollback dialog copy is now evidenced in `ui/src/routes/workspace/admin/versions-helpers.ts` and `ui/src/routes/workspace/admin/versions-helpers.test.ts`, root live-header badge/notification copy is now evidenced in `ui/src/components/layout/root-layout-helpers.ts` and `ui/src/components/layout/root-layout-helpers.test.ts`, and builder authoring-mode helper defaults are now evidenced in `ui/src/routes/workspace/admin/page-manifests/editor-builders.ts` and `ui/src/routes/workspace/admin/page-manifests/editor-builders.test.ts`, but subscription scoping, replay-window semantics, remaining realtime browser proof, and other open runtime surface items are still required before completion.
- Live backend evidence round (2026-08-27): `curl http://127.0.0.1:8000/api/v1/ping` returned `200 {"message":"pong","version":"dev"}`; `curl -H 'Host: airtime.local' http://127.0.0.1:8000/s/airtime/workspace` returned the workspace SPA; `curl -H 'Host: airtime.local' http://127.0.0.1:8000/api/v1/kernel/_registry` returned `401` before auth and then `200 {"commands":[],"site":"airtime.local"}` after tenant login; `curl -H 'Host: airtime.local' http://127.0.0.1:8000/api/v1/system/page-manifests` returned `200 {"data":[]}`; tenant login at `POST /api/auth/login` returned `403 email_verification_required` until `_kora_user.email_verified_at` was set in the live tenant DB, after which the same request returned `200` with a `kora_sid` cookie. Console login remained unverified in this round.
- Builder browser evidence round (2026-09-12): authenticated disposable `live-test` workspace loaded `/workspace/admin/page-manifests/new` through the Studio proxy; the builder started with a generic empty manifest, required a name, allowed a non-domain-specific `Franken card` layer, saved a draft, and displayed publish-preflight validation for the unsafe test route. The manifest list remained limited to the existing persisted entry, so draft persistence/list reconciliation and browser publish proof remain open; no domain-specific builder default was introduced.
- Live site-backed evidence round (2026-08-27): started the server with `--site airtime-uat.local` and the MySQL platform env so the tenant loaded from DB; `GET /api/v1/ping` returned `200 {"message":"pong","version":"dev"}` for `Host: airtime-uat.local`; `POST /api/auth/login` with `admin@airtime-uat.local` succeeded after normalizing the tenant password in `airtime-uat_local` and returned a `kora_sid` cookie; `GET /api/v1/system/doctype/Product` with the session cookie returned the full Product schema; `POST /api/resource/Product` created `PROD-0001` and `PROD-0002` from the same payload, showing duplicate creates produce distinct docs; `POST /api/resource/Customer` created `CUST-0001`; `POST /api/resource/Order` created `ORDE-0001` with one item; `POST /api/resource/Order/ORDE-0001/workflow_action` with `action=Confirm Order` returned `200` and advanced `status` from `Draft` to `Confirmed`; `PUT /api/resource/Product/PROD-0002` updated the record; `DELETE /api/resource/Product/PROD-0002` returned `200 {"data":{"message":"deleted"}}`; CSRF was satisfied via `kora_csrf` plus `X-Kora-CSRF-Token`, and bearer-token-only attempts on the tenant resource routes were rejected as unauthenticated.
- Workflow evidence round (2026-08-27): `GET /api/v1/system/workflows` returned `200` with one workflow (`Order Fulfillment` on `Order`); `GET /api/v1/system/workflows/Order` returned `200` with the same workflow payload; `GET /api/v1/system/workflows/Sales%20Order` returned `404 No workflow for Sales Order` because that doctype is not present in the live tenant.
- Workflow CRUD round (2026-08-27): after patching `configstore.SaveWorkflows` to normalize `allow_edit` before persistence, a disposable `ApiSmoke3` fixture verified the full live path: `POST /api/v1/system/doctype` + `POST /api/v1/system/workflows` returned `200`/`201`-class success, `POST /api/resource/ApiSmoke3` returned `201`, `PUT /api/resource/ApiSmoke3/APIS-0001` returned `200`, `POST /api/resource/ApiSmoke3/APIS-0001/workflow_action` returned `200` and moved `status` from `Draft` to `Published`, `DELETE /api/resource/ApiSmoke3/APIS-0001` returned `200`, `DELETE /api/v1/system/workflows/ApiSmoke3` returned `200`, and `DELETE /api/v1/system/doctype/ApiSmoke3?cleanup=full` returned `200`. The earlier workflow-save failure was the live symptom that triggered the fix.
- Phase 5: offline sync, retention, conflict, and reconciliation tests pass in `contract` and `ui`.
- Phase 6: Cloud registration, bootstrap, validation, rollout, identity, routing, provisioning, deletion, and evidence tests pass in `cloud`.
- Operation kernel slice (2026-08): `kernel/` integration suite passes 17 scenarios against the MySQL harness — happy-path atomic commit (row + receipt + audit + outbox), idempotent replay and key reuse, tenant isolation with cross-tenant denial, authorization parity across HTTP/SDK/MCP/AI/CLI sources, stale-version conflict with failure audit, rollback atomicity on unique violation, unknown-field rejection, outbox delivery retry/restart/receipt dedup, local WAL provider delivery, embedded NATS JetStream end-to-end delivery, and audit-ledger redaction (hashes only, no payloads). Kernel SQL is dialect-aware via new `db.Rebind` bridge. Known gap feeding DB-001: the ORM still emits raw `?` placeholders, so full PostgreSQL DML enablement requires migrating orm/ to `db.Dialect.Placeholder()`; PG DDL is already complete.
- KERNEL-008 implemented: config-defined command resources (`kernel/command_resource.go`, `kernel/command_exec.go`) — YAML definitions with typed input record, multi-step create/update transactions with `$input` references, per-step least-privilege authorization (create steps → "create", update steps → "write"), emitted events into the in-tx outbox, strict unknown-key parsing; executed through the identical kernel UoW (receipt/audit/outbox atomic); introspection at `GET /api/v1/kernel/_registry`; 4 integration tests. Registry now loads from `KORA_COMMANDS_DIR` at startup (`kernel/command_load.go`, fail-closed on invalid definitions) and is injected into the API handlers via `RegisterRoutesOnGroupWithAnalytics`; `kora validate`-style CLI checks land with MIG-006.
- KERNEL-009 implemented: audit before/after state hashes (`CanonicalDocHash`) on all three dialect DDLs; verified by happy-path + redaction tests.
- Backlog tooling: kora-cloud CI now runs `cmd/ticketlint` over all 117 tickets (READY-030); traceability index maps spec invariants/scenarios → tickets or non-goals (ARCH-001).

## Suggested Work Queue

1. Finish the shared executor coverage for Phase 3A.
2. Lock down workflow actor fencing, timers, and recovery tests.
3. Push AI run persistence and resume/cancel semantics to completion.
4. Tighten capability status and parity tests for current projections.
5. Complete Phase 4A: visual builder/runtime parity, deterministic responsive layout, and live-page reconnect behavior.
