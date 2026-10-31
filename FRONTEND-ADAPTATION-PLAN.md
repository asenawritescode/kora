# Frontend contract migration

Date: 2026-10-13
Status: target; source changes are scheduled in G8 of the [architecture specification](docs/architecture/target-state-spec.md).

## Clients

`kora-studio-mvp` is the standalone workspace UI. `kora/ui` is the embedded distribution. Both use the same Engine contracts. `kora-cloud/web` manages Cloud operations. `kora-ts-sdk` owns the framework-independent TypeScript client. Product display naming remains governed by the brand documentation.

## Required adaptation

1. Consume generated DTOs, enum sets, runtime decoders and fixture manifests from an Engine contract release. Keep presentation models local; remove duplicate fixed wire models after clients migrate.
2. Preserve same-origin session cookies, CSRF protection, tenant path prefixes and runtime-injected endpoints. Never include service credentials in browser configuration.
3. Add a business action inbox with needs-action, sent, waiting and completed views. Decision commands carry assignment ID, expected revision and a stable idempotency key. Actor identity comes from the server session.
4. Keep AI execution approvals separate from business requests. Keep the current document-state editor separate from durable orchestration definitions. Share presentation elements only when semantics are actually the same.
5. Use realtime for authorized invalidation and progress. Refetch protected data through the read API. Close streams and clear tenant-scoped cache on tenant switch or revocation. Do not merge full or old document state from untrusted event payloads.
6. Use capabilities to expose supported features. New UI against an older Engine disables absent request/orchestration capabilities rather than guessing endpoint behavior.
7. Retain manifest-first rendering, bounded expression evaluation and component contracts. Schema-defined data remains a validated extension point; fixed approval/control commands use concrete generated types.
8. Separate shell state from server state. Server state uses tenant/actor/revision-scoped query keys. Cancellation, stale approval and uncertain external outcome have explicit user-visible states.
9. Initial approvals require an online authorized response. Local drafts may persist, but offline decision replay is not enabled without a separate conflict and reauthorization contract.

## Acceptance

Generated contracts and fixtures match in Engine, SDK, Studio and Cloud. Test two-user request completion, revoked permissions, stale subjects, concurrent decisions, duplicate clicks/retries, notification outages, tenant switching and realtime reconnect. Test standalone self-hosted operation without Cloud, same-origin proxying, keyboard navigation and mobile layouts.

Remove compatibility clients only after the release window and observed consumer migration in specification section 12. The embedded UI is not removed until a separate distribution-parity decision passes; source duplication is not resolved by breaking self-hosted installation.
