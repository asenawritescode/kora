# Engine worker and background SQL admission inventory

Audited against the Engine source tree on 2026-10-15. This inventory covers
production worker compositions, lifecycle-owned background work, and CLI
maintenance entry points. It distinguishes SQL admission from in-memory,
network, and future workers so later groups are not pulled forward implicitly.

## Engine-composed work

| Worker or path | Owner and bound | SQL admission boundary | Other pressure boundary / notes |
|---|---|---|---|
| Tenant analytics flush (`analytics.Worker`) | Tenant runtime generation; cancellation and join through runtime close | One ordinary endpoint-shared permit around the flush transaction; monthly rollup and projection retention each acquire from the heavy-maintenance lane | WAL is authoritative; bounded wake-up and subscriber queues have process byte reservations |
| Analytics rebuild (`analytics.RebuildRunner`) | One runner per resident tenant; durable job table is cold-tenant discovery authority | Ordinary permit for claim; one heavy-maintenance permit for each rebuild execution/transaction | One active rebuild per tenant; fenced lease, retryable durable job, cached counters/backlog |
| Outbox publisher (`RunOutboxPublisher`) | Tenant runtime generation | Separate permit for each selection, claim, attempt lookup, payload lookup, retry/terminal update, and backlog query | Provider publish runs after SQL admission is released; batch and payload limits; fenced-lease-loss counter |
| Local outbox dispatcher (`RunWebhookOutboxDispatcher`) | Tenant runtime generation | Dispatcher admits selection and receipt/local transaction; webhook adapter admits its own SQL phases | HTTP delivery does not retain a DB permit; bounded batch/bytes and transactional receipt where configured |
| Durable scheduled occurrences (`scheduler.Scheduler`) | Tenant runtime generation; its internal workers are canceled and joined | One active-work permit across a scheduled local unit, with bounded DB lease; backlog sampler acquires separately | Bounded job concurrency, occurrence persistence, lease fencing, uncertain outcomes and sampled backlog |
| Human request/workflow recovery (`HumanWaitRecoveryWorker`) | Tenant runtime generation | One permit per bounded pass (request expiration and terminal/timer/signal continuation batches) | Batch maximum 500; durable scanner wakes unloaded tenants |
| Tenant session cleanup (`auth.SessionManager`) | Tenant runtime; cache-only sweep is in memory | One permit for the paired expired-session and magic-link cleanup statements | Cache entry and bytes are locally and process-wide bounded |
| Process session cleanup (`auth.SessionManager`) | Process resource owner; canceled/joined before platform DB close | Same paired-cleanup admission against the platform endpoint | Cache-only sweep is in memory |
| Authenticated tenant HTTP and request API | HTTP request lifecycle | Router middleware admits after tenant resolution and before session authentication; handlers reuse the request permit | Bounded active requests/waiters/deadline; stream and upload bodies use separate budgets |
| Tenant activation/reload | Engine runtime coordinator | Platform directory operations and tenant activation/connect/bootstrap/schema work are separately admitted; no nested platform/tenant permits | Activation waiters and resident/retiring runtimes are bounded |
| Directory consumer and optional NATS relay | Process owner | One platform permit per registry call; callbacks run after release; relay inherits the same admission | Bounded pages, cursor, wake queue and retry cadence |
| Cold durable-work scanner | Process owner | Separate platform permit per scan operation and one tenant endpoint permit for each cold probe | Bounded page size, at most one cold tenant probe at once, persisted cursor and capacity retry |
| Directory lag monitor and platform readiness | Process owner / request | Platform endpoint permit per query/probe | Bounded context and interval; no tenant or SQL labels in metrics |
| Pending deletion recovery | Process owner; joined before platform DB close | One platform permit for each bounded retry pass | Bounded timeout/retry interval; attempts, failures and admission deferrals counted |
| Onboarding background provisioner | `ConsoleHandler`; at most 32 concurrent jobs, canceled and joined at shutdown | Platform/tenant work admitted around each persistence, lookup and installation unit | Provisioning intent persists; automatic restart resumption is a later G5/G7 recovery gate |
| Console/background site database provisioning | Synchronous console handler or bounded onboarding worker | The console route/worker holds platform work admission; Engine composition adds tenant maintenance admission when the endpoint differs and reuses the existing platform permit when both endpoints are the same | Capacity exhaustion returns a retryable HTTP response or keeps the durable onboarding job retrying; CLI `kora setup` reserves both endpoints and deduplicates a shared endpoint. |
| Async script hooks | Process-owned bounded queue and consumer | Tenant work is acquired after resolving the pinned tenant runtime and before the script's SQL work | Count, estimated bytes, item bytes and script concurrency/time are bounded; runtime handle pins DB until completion |
| CLI `site-directory`, analytics, migration, config, and initial setup commands | Command context | Platform lookups and tenant operations admitted; setup site provisioning reserves platform and tenant maintenance endpoints once each (deduplicated when shared), then config/schema import uses the tenant heavy-maintenance lane | Directory listing keyset-pages at 500; output streams instead of materializing all tenants |

## Bounded work without SQL

| Component | Ownership and bound |
|---|---|
| Analytics `MultiBus` fanout and local realtime queues | Tenant-owned and joined; listener count and conservative queued bytes capped; queue age is reported as an upper bound |
| NATS realtime bridge and outbox side-effect consumer | Tenant-owned and joined; application queues/subscriptions bounded; durable SQL source remains replay authority |
| Cloud analytics relay | Tenant-owned and joined; bounded subscriber queue and HTTP client timeout; it performs no tenant SQL |
| System-session cleanup | Process-owned and joined; it only removes expired in-memory entries |
| Script runner | Tenant-specific active execution and waiter limits, global tenant-gate cap, and context timeout; Goja `MaxRAM` is not represented as hard enforcement |

## Admission-aware workers staged for later groups

`notifications.NewWorkerWithAdmission` requires an admission controller and
admits each claim/accept/retry SQL phase, but Engine does not yet compose it.
Its durable notification service and composition are G5 work. The durable
`workflow.ExecutionWorker` likewise requires work admission and bounded
concurrency, but Engine composition and full execution/reconciliation are G6
work. Neither is an active Engine SQL path today; the scanner can still find
their durable rows where applicable. Their admission-aware constructors are
not evidence that the future features are shipped.

## Audit boundary

This is a source-composition audit, not a distributed quota claim. Configured
per-replica allocations must sum within the physical database-server budget;
no in-process semaphore coordinates other replicas. The inventory does not
authorize deleting durable rows. Outbox history, analytics receipts/checkpoints,
local WAL, broker redelivery, and backups still lack one safe retained source
floor. Replay retention remains disabled until a monotonic floor, backup/restore
validation, every replay-source acknowledgment, and resumable cross-store
pruning are implemented and tested. PgBouncer and ProxySQL are not recommended
or considered validated.
