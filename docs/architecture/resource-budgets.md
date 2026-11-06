# Process and database connection budgets

`database/sql` limits each `*sql.DB` separately. Engine production database
opens now also use `internal/dbaccess`, which shares physical-connection
permits across tenant, platform, command, and administrative pools targeting
the same endpoint. Each process defaults to at most 64 live connections per
endpoint. Connector admission allows at most twice that many waiters by
default, with a 1.5 second wait deadline. A canceled context ends the wait
early. Rejection and wait counters are available from the budget snapshot API.
The endpoint-budget registry is limited to eight physical endpoint identities
per process by default; set `KORA_DB_MAX_BUDGET_ENDPOINTS` to a larger positive
value if the deployment intentionally connects to more servers. Each open
`*sql.DB` holds an endpoint reference; the slot is released only after the last
pool and its last physical driver connection close. A caller-held `sql.Conn`
continues to count against the shared budget after `DB.Close`, so a replacement
pool cannot temporarily split the endpoint cap. Safe lazy-tenant retirement
therefore reclaims the endpoint limit after in-flight connection use drains.

Operators can divide a known physical-server limit among Engine replicas. Set
the same values on every replica:

```sh
KORA_DB_SERVER_MAX_CONNECTIONS=240
KORA_DB_SERVER_RESERVED_CONNECTIONS=40
KORA_DB_ENGINE_REPLICAS=4
```

Engine allocates `floor((240 - 40) / 4) = 50` physical connections to each
replica and endpoint. The default process cap of 64 applies when the physical
server maximum is unset. `KORA_DB_CONNECTION_BUDGET` can lower that process
cap. When a server maximum is configured, the effective cap is the lower of
the divided server allowance and `KORA_DB_CONNECTION_BUDGET`. `KORA_DB_CONNECTION_WAITERS` and
`KORA_DB_CONNECTION_WAIT_MS` override the default waiter count and deadline.

Platform console database work uses the platform endpoint admission: public onboarding requests and job reads, authenticated site create/update/delete/password-reset operations, fallback site discovery, and unmanaged health probes. A managed site health probe instead uses that tenant generation’s admission. Self-service provisioning runs at most 32 concurrent jobs per Engine process; each job waits with cancellation for one platform work permit before its database-backed provisioning unit. This is a per-process worker bound, not a fleet-wide provisioning limit. Site creation releases platform admission before runtime installation; the created-site directory lookup and subsequent provisioning-state writes reacquire their own platform work unit. This avoids nested acquisition when platform and tenant pools share one endpoint budget. A process shutdown joins these jobs; automatic resumption of an interrupted onboarding operation after restart remains a separate durable-operation recovery gate.

Tenant activation also acquires its endpoint-shared work permit for analytics
schema bootstrap and durable scheduler schema creation. Analytics bootstrap now
uses the activation context for every SQL call, so cancellation or an admission
deadline stops the schema operation before tenant services are published.

The `site-directory` CLI uses endpoint-shared work admission for platform
health pings, directory reads, assignment changes, migration, and pruning.
Migration and pruning use the separate heavy-maintenance lane. Directory list
reads use 500-row keyset pages and stream the existing JSON-array output, while
the read-only probe admits platform discovery and each tenant connect/ping
independently; its worker concurrency remains capped at eight.
The analytics CLI admits tenant backfills on the heavy-maintenance lane and
analytics status reads on ordinary work admission. The schema migration CLI
admits each tenant connection, bootstrap, config load, and migration as one
bounded maintenance unit; platform discovery is separately admitted.
Config export/version/diff commands use ordinary tenant admission. Config
imports and local rollback use the heavy-maintenance lane; platform discovery
and each tenant lookup are separately admitted and cancellation-aware.

The same endpoint budget also owns an active-work admission limiter. By
default it admits at most `min(connection budget, 16)` API requests at a time,
queues at most twice that many requests, and waits for up to one second.
`KORA_DB_ACTIVE_WORK_BUDGET`, `KORA_DB_ACTIVE_WORK_WAITERS` and
`KORA_DB_ACTIVE_WORK_WAIT_MS` override those limits. The Engine admits routed
tenant requests after tenant resolution and before session authentication, so
auth and handler database operations share one slot; nested queries and
transactions do not reacquire capacity. Realtime and authenticated attachment
GET/HEAD routes acquire a short permit for session authentication, then release
it at the handler boundary before replay or storage streaming begins. Stream
lifetimes use their separate realtime or storage bounds. Public file GET/HEAD
routes do not authenticate and remain stream-exempt. File deletion is a
bounded mutation and remains under tenant work admission. Upload handling uses
its independent weighted byte reservation while parsing and processing bodies.

Heavy maintenance work uses a separate endpoint-shared lane. By default, one
heavy job is allocated per Engine replica when active-work capacity is greater
than one, leaving at least one ordinary-work slot. Set
`KORA_DB_SERVER_MAX_HEAVY_JOBS` to the fleet-wide configured maximum and
`KORA_DB_ENGINE_REPLICAS` to the same replica count on every Engine; Engine
divides the heavy-job allocation evenly and rejects a nonzero allocation that
leaves a replica with no slot or consumes all of that replica's active-work
capacity. `KORA_DB_ACTIVE_WORK_BUDGET` must therefore be greater than the
per-replica heavy allocation. Analytics rebuilds, monthly rollups, and
projection-retention cleanup use this lane. This is static operator allocation,
not a distributed semaphore; all replicas must be updated together when replica
count changes. Maintenance active/capacity gauges are
exported separately from general active-work metrics.

The endpoint key is derived from the driver and database endpoint. When DNS
aliases or proxy addresses reach one physical backend, set the same
`KORA_DB_BUDGET_KEY` on the participating Engine processes so their local pools
use one allocation identity. Do not put credentials in this value.

NATS realtime diagnostics report pending message and byte counts from the
client subscription buffer, plus the span between consecutive diagnostic
samples that observed a nonempty buffer. Core NATS does not expose each
buffered message's enqueue time, so this observed span is not message age. The
separately reported Engine application-channel age is an upper bound based on
its locally observed enqueue time.

`kora.analytics.wal.cold_bytes` reports the summed bytes for unloaded active
tenant WAL directories observed during the last completed directory sweep.
`kora.analytics.wal.cold_bytes_unknown` is set when that sweep resumed from a
persisted mid-page cursor or any tenant WAL probe failed, so a partial total is
never presented as complete. These gauges are separate from resident WAL
bytes; tenant activation can move a directory between samples, so do not add
the two values as an instantaneous process total.

These settings are a static allocation, not a distributed semaphore. Every
replica must use the same server maximum, reserve, replica count, and endpoint
identity. A replica-count change requires updating the allocation before the
new replica opens connections. Connections from other applications, poolers,
and operational tools must be included in the reserve or removed from the
Engine allocation. `db_max_open_conns` remains a per-tenant-pool maximum; it is
not the physical-server limit.

## Analytics subscriber queues

Every Engine process shares one conservative byte admission budget across all
resident tenant analytics wake-up channels and `MultiBus` subscriber queues.
It defaults to 512 MiB and is configurable with
`KORA_ANALYTICS_PROCESS_MAX_QUEUED_BYTES` from 1 byte through 4 GiB. For
backward compatibility, if this process-specific variable is unset, Engine
uses `KORA_ANALYTICS_MAX_QUEUED_BYTES` for the process budget as well. New
deployments should set the two separately: `KORA_ANALYTICS_MAX_QUEUED_BYTES`
controls each tenant's in-memory wake-up queue (64 MiB default, up to 1 GiB),
while `KORA_ANALYTICS_PROCESS_MAX_QUEUED_BYTES` controls aggregate occupied
payload bytes across the process. Each queued event
reserves the bus's maximum accepted payload size, so the budget does not
depend on caller payload estimates. It accounts queued payload reservations,
not channel/runtime metadata or the event temporarily held by the fan-out
goroutine. Tenant listener-count and per-listener byte bounds continue to
apply independently.

When the process budget is full, the durable WAL still accepts events but may
skip their in-memory wake-up hint; the analytics worker continues scanning
WAL on its timer. Ordinary subscribers drop that delivery and remain
registered; realtime listeners are closed and removed so clients can
reconnect and refetch authorized state. `kora.analytics.process_queued_bytes`,
`kora.analytics.process_queue_max_bytes`, and
`kora.analytics.process_queue_rejected` expose process usage,
capacity, and cumulative rejected queue reservations. This is a per-process
cap; replicas do not coordinate queue bytes.

The same process-owned admission also caps registered analytics listeners
across all resident tenant buses at 4,096 by default. Set
`KORA_ANALYTICS_MAX_SUBSCRIBERS` from 1 through 65,536 to change this process
limit; the existing per-tenant limit remains 128. Listener registration is
rejected when either cap is full. Removing a listener, closing a tenant
generation, or evicting a lagging realtime listener returns its process slot.
`kora.analytics.listeners.process_active`,
`kora.analytics.listeners.process_capacity`, and
`kora.analytics.listeners.process_rejected` expose this count budget. The
separate queued-payload byte limit still accounts for occupied slots; this
count cap also bounds allocated listener channels when they are empty.

Analytics flushes and WAL replay batches default to 100 events and have a hard
maximum of 500. `KORA_ANALYTICS_BATCH_SIZE` values above the maximum are capped,
and `NewWorker` enforces the same limit for programmatic configuration. This
bounds event IDs and contributions retained for one SQL transaction; it does
not bound the durable WAL or correctness-receipt history.

## Optional JetStream retention

When NATS is configured, a newly created Engine event stream uses file-backed
JetStream storage and has a 1 GiB and 1,000,000 message cap by default. Operators
can set `KORA_NATS_STREAM_MAX_BYTES` from 1 byte through 64 GiB and
`KORA_NATS_STREAM_MAX_MESSAGES` from 1 through 100 million. Configure the NATS
server's JetStream `store_dir` on persistent storage; file-backed stream state
survives a broker process restart only while that store remains available.
Back up and restore that directory using the operator's NATS recovery procedure.
NATS remains optional; self-hosted Engine operation without a broker is
unchanged.

Dead letters are published into the event stream under
`KORA_NATS_DEAD_LETTER` (default `<prefix>_deadletter`). The subject lies
outside the `<prefix>.>` event tree, so no consumer filter on that tree
re-consumes it, and bootstrap adds it to the stream's subjects (an existing
stream that captures only `<prefix>.>` is updated in place). Configuration
validation rejects a dead-letter subject that the consumer filter matches.
Dead letters stored under the previous default `kora.deadletter` stay in the
stream (no data migration is performed). Inspect them with the `nats` CLI and,
once handled, remove them with
`nats stream purge <stream> --subject kora.deadletter`.
Operators who set `KORA_NATS_DEAD_LETTER=kora.deadletter` explicitly must unset
it or choose a subject outside `<prefix>.>`. Dead letters share the stream's
byte and message caps.

Bootstrap upgrades a same-name stream only after checking its subject,
retention mode, and current backlog; it preserves settings such as replica
count. If the retained backlog already exceeds a requested cap, or the stream
has age-based expiry, startup fails and requests an operator migration instead
of trimming events. Event streams use `DiscardNew` and have no age-based expiry:
when capacity is reached, new JetStream publishes fail instead of evicting
retained events. The SQL outbox therefore does not receive a publish
acknowledgment for that delivery; after bounded retries, the row becomes
visibly failed and remains available for operator recovery. All Engine
replicas sharing a stream must use the same limits.

JetStream does not permit changing a stream's storage type in place. For
compatibility, bootstrap leaves an existing `MemoryStorage` event stream
memory-backed and reports its actual storage in provider diagnostics. The
provider rejects new durable event publishes to that stream, so the Engine
outbox does not mistake a memory-only acknowledgment for durable acceptance;
rows remain retryable or visibly failed until migration. Migrate the stream
and its durable consumer state through an operator-validated backup/restore
or replay procedure before relying on restart-durable delivery or retiring the
compatibility path. Do not delete the old stream until message and consumer
recovery have been demonstrated.

The site-directory stream is a separate wake-up channel backed by the durable
SQL directory feed. It is capped at 64 MiB and 100,000 messages with a 24-hour
age limit and may discard old hints; consumers resume from their SQL feed
cursor, so no directory change depends on retaining a broker message forever.

## Database compatibility evidence

The repository currently implements MySQL, PostgreSQL, and LibSQL dialects.
On 2026-10-15, disposable integration checks ran against MySQL 8.4.11,
PostgreSQL 16, and LibSQL sqld 0.24.33 using its HTTP transport. The checked
packages were `analytics`, `outbox`, `site`, `requests/sqlstore`, and
`workflow/sqlstore`; coverage included rebuild/lease and receipt persistence,
site directory lifecycle and the 10k-row MySQL directory projection, request
schema/read, and workflow claim/timed-wait recovery. The exact command and
result are recorded in `implementation-status.md`.

This is evidence for those operations and versions, not a complete support
matrix for every SQL statement, migration, or server release. MariaDB is not a
separate declared matrix entry. PgBouncer and ProxySQL compatibility have not
been tested, so pooled endpoints are not recommended by this evidence.

## Durable event storage and replay retention

The current implementation does not enforce one maximum replay age across
analytics WAL files, `_kora_outbox` rows, consumer receipts, rebuild
checkpoints, NATS redelivery, and database/filesystem backups. Published outbox
rows and analytics receipts/checkpoints are not pruned; local WAL recovery files
have no disk-size cap and remain until their events commit and acknowledgment
finishes. New analytics WALs use a stable tenant-ID namespace. Startup migrates
pending legacy name-keyed directories when their name is the tenant's current
canonical name or a registered alias; an unassigned or conflicting pending
directory prevents startup and remains untouched for operator recovery. A crash
during WAL acknowledgment leaves a `.wal-ack-*` recovery file that is replayed
as pending data. NATS event-stream limits reject new events rather than evict old ones.
The self-hosted backup now carries a versioned source-sequence high-water
checkpoint per MySQL database, and restore validates that every recorded
database exists with at least that sequence. Source sequence now uses a
tenant-database-scoped singleton
that remains monotonic across route-name changes and seeds from legacy route-keyed
sequence rows. This fixes rebuild cutoff ordering across tenant renames but does
The checkpoint validates the selected backup's restored database contents; it
does not identify the oldest backup still restorable or validate NATS/WAL replay
sources, so it does not establish a replay-retention horizon. Release must drain old event writers
before enabling the singleton allocator because old versions do not coordinate
on that row. As a result, disk usage for retained durable event history
can grow without a configured bound. Operators must monitor DB and WAL volume
capacity and provision space for backlog and recovery; the memory and queue
limits above do not cap this durable storage.

Do not add age-based deletion for outbox rows, receipts, checkpoints, or WAL
files until a coordinated retention protocol is implemented and tested. A safe
cutoff must be monotonic per tenant and no newer than every remaining replay
source: acknowledged outbox/consumer state, local WAL recovery, broker
redelivery, and the oldest restorable backup. Each self-host backup records its
database high-water checkpoint; `selfhost-source-manifest.py backup-floor`
computes a backup-only minimum from the operator-supplied complete retained
manifest inventory, and `validate-backup` checks a selected restore against that
catalog. The operator must ensure every restorable copy is represented and
retired copies are unavailable before advancing it. This does not include the
other replay sources or authorize pruning. Restore must reject or explicitly
reconcile a backup older than the eventual coordinated replay floor. Deletion
must be resumable and
must advance the floor only after all participating stores confirm completion.
Until then, correctness records are intentionally retained indefinitely; this
is a known durable-storage growth risk, not a bounded-retention guarantee.

The current source identities do not yet compose into that floor:

| Replay source | Durable identity and completion fact | Missing input for a common floor |
|---|---|---|
| Analytics WAL | Stable tenant-ID directory and event ID per record; `AcknowledgeWAL` removes committed IDs after the analytics transaction | No durable contiguous source-sequence acknowledgment; legacy and direct events can omit sequence |
| Tenant outbox | Monotonic tenant `source_sequence`; `status` records external publication, while `_kora_outbox_receipt` records each local consumer by event ID and stores the delivered source sequence when known | Legacy receipts retain `NULL`; no deletion watermark exists, and every applicable consumer receipt must participate independently |
| Analytics receipts | `(site, event_id)` deduplicates rollup contributions; schema v5 stores the optional source sequence on newly processed events | Legacy receipts and direct events may have no sequence; there is still no contiguous acknowledgment projection, and not every event necessarily has an outbox row to join |
| Rebuild checkpoints | `last_source_sequence` per site, doctype, and date range | A range cutoff is not a global consumer acknowledgment or a safe receipt-deletion watermark |
| NATS event stream | JetStream stream sequence, bounded with reject-new; consumer settlement is tracked per delivery | Broker sequence is not the tenant source sequence, and publication acknowledgment does not mean all consumers completed |
| Self-host backups | Per-database source-sequence high-water in each manifest; operator catalog computes minimum among supplied retained manifests | Inventory completeness and retirement of remote copies remain operator assertions; the catalog does not include live replay sources |

Consequently, no component may advance a shared floor using only its local
completion fact. In particular, an outbox publish acknowledgment, a rebuild
checkpoint, or WAL byte/age telemetry is insufficient to authorize deletion.
The current implementation intentionally has no cross-source floor advancement
or pruning operation.

The connector budget caps established driver connections. Active-work admission
covers routed tenant requests and the currently assembled analytics, scheduler,
webhook, outbox, and workflow-recovery workers. Durable scheduler claim, renewal,
and terminal-state SQL each acquire a permit; the claim permit is released before
job execution. Webhook dead-letter selection and each retry-state write are
separately admitted. Outbox publication, metrics, backlog samples, and operator
replay admit their SQL phases without holding a permit across destination calls.
The current Engine composition audit is recorded in the
[worker/admission inventory](worker-admission-inventory.md). Durable notification
delivery and workflow execution are not yet composed by Engine; their bounded
constructors do not mean those later G5/G6 capabilities are shipped.
`database/sql` may still queue calls behind a saturated individual pool.
Session-dependent bootstrap and deletion locks must keep their separately
capped direct or session-pooled administrative route; do not place those locks
behind a transaction-pooling endpoint without the compatibility tests in the
target specification.

Platform `/health` and `/readyz` database probes use a two-second context
deadline and the admission limiter for the endpoint they probe. Readiness
capacity rejection reports `not_ready`; health keeps its stable degraded
response and skips the ping. Engine health probes use the platform database,
not the startup primary tenant database. The optional primary-database
compatibility fallback remains for explicit assembly and must be paired with
that endpoint's own admission.
The protected tenant-reload endpoint admits each platform directory lookup
separately, uses context-aware SQL, releases platform capacity before tenant
activation, and returns 503 when the platform limiter is full.
Credential-free management inspection admits the descriptor/alias read and
directory-revision read separately through the same platform limiter.

The platform `sql.DB` pool is separately capped by
`KORA_PLATFORM_DB_MAX_OPEN` (default 4, accepted range 1–256). This pool cap
limits how much of the endpoint-shared connector budget platform operations can
occupy; it does not add another allocation to the physical server budget.
Pending-deletion recovery acquires one unit from the same endpoint-shared
active-work admission as tenant work before its bounded retry pass. Resource
metrics expose cleanup attempts, retry failures, and work-admission deferrals
without tenant or deletion identifiers.
Periodic process and tenant session cleanup similarly acquire one unit for the
paired expired-session and magic-link cleanup statements; cache-only sweeps do
not acquire database work admission. The optional directory-lag monitor also
acquires one unit around its single platform query. Newly provisioned Engine
tenants keep the session sweeper paused until the runtime installer attaches
admission and starts its workers. The durable directory feed acquires one permit
around each individual registry operation and uses context-aware SQL variants in
the production registry; the legacy registry methods remain source-compatible
wrappers. It releases the permit
before applying each change or snapshot, because callbacks can activate tenant
runtimes and use their own admission. The optional NATS relay inherits the same
endpoint admission. Compatibility registries without the context-aware extension
remain bounded by active-work admission, but cannot cancel an in-flight legacy
call.

Analytics flush transactions, monthly rollups, and retention cleanup each use
one permit from the endpoint-shared active-work limiter. A rejected flush leaves
its accumulated deltas and event receipts pending for the next pass. Scheduled
Scheduled jobs acquire admission for their database-backed work. Outbox
publication admits each bounded SQL phase separately and releases admission
before calling the configured destination. Durable local dispatch admits its bounded
selection query, each consumer receipt query/write, and each local transactional
effect separately; it closes the selection cursor before invoking consumers.
Webhook subscription reads and delivery-log/stat writes acquire tenant admission
around their SQL, while extension HTTP calls hold no database permit. When
`KORA_OUTBOX=false`,
event-driven integrations, including webhook delivery, are disabled; Engine does
not fall back to a lossy in-memory webhook listener.
Outbox publication and local consumer dispatch default to 100 events per pass
and reject requested batches above 500. Local dispatch also buffers no more
than 48 MiB of serialized event payload per pass; it may transiently scan one
additional event, whose payload is capped at 16 MiB. New outbox events are capped at 16 MiB
of serialized envelope data before tenant sequence allocation, then checked
again after the writer assigns the sequence. These limits bound new writes and
each delivery batch; existing persisted outbox payloads remain readable during
compatibility migration and are not rewritten or deleted by this limit.
The process-owned scanner admits each platform cursor load, bounded directory
page read, cursor advance, schema check, and credential descriptor lookup
separately. For each cold tenant it then obtains one permit from that tenant's
endpoint-shared active-work limiter before opening/pinging the capped DB pool,
and holds it through optional scheduler schema/definition work and durable-work
queries. Platform and tenant permits are not nested. Local scanner tests cover
admission rejection/release; disposable cold-probe integration coverage still
requires configured fresh database endpoints. Activation admission wait
metrics are exposed.

Configured tenant scheduler jobs persist a definition digest, the exact job
configuration for each occurrence, a scheduled timestamp, attempt count, and
fenced owner lease in `_kora_schedule_definition` and
`_kora_schedule_occurrence`. The SQL store currently supports MySQL, PostgreSQL,
and LibSQL. A failed execution or expired lease is recorded as
`outcome_unknown`; it is not automatically replayed because a handler may have
completed an external effect before the process lost its lease. Operator
reconciliation is not implemented yet. A process-owned durable-work scanner
walks credential-free directory pages, checks one cold tenant database at a
time under endpoint-shared admission, and wakes tenants with due schedule
occurrences, outbox/webhook work, workflow signals/timers, request deadlines,
notifications, analytics rebuild jobs, or retained local analytics WAL. The
scan cursor and completed-sweep timestamp persist in the platform database.
This scanner is a recovery hint; tenant SQL and local WAL remain the durable
sources of truth. Probe failures are retried on later sweeps and exposed as
metrics.

Tenant pools are opened lazily on first routed request. Engine caps resident
tenant runtimes at 256 by default; set `KORA_MAX_RESIDENT_TENANTS` to a
positive integer to change the per-process limit. Credential lookup/runtime
construction and directory replacement are serialized by the tenant runtime
coordinator. It admits at most 32 waiting operations by default and bounds
each wait to 1.5 seconds. `KORA_TENANT_ACTIVATION_WAITERS` and
`KORA_TENANT_ACTIVATION_WAIT_MS` override those limits. A canceled request
stops waiting immediately. Saturation or timeout returns HTTP 503 with
`Retry-After: 1` and `activation_capacity`; an active operation is not canceled
by a waiting request. Metrics expose current/capacity waiters, cumulative waits
and durations, and rejected operations.
When the resident cap is full, Engine fences and retires the least-recently-used
idle tenant generation, closes it, and then admits a cold tenant. In-flight
runtime handles are never evicted. If retirement or close fails, the capacity
reservation remains held and activation fails closed; the scanner can
reactivate the retired tenant when durable work is due. The cap bounds resident
pools but does not guarantee immediate service while all generations are busy
or retiring. Metrics expose active/retiring runtimes, configured capacity, and
scanner probes, failures, due tenants, wake failures, capacity rejections and
last completed sweep age.

Async script-hook delivery uses a process-owned in-memory queue capped at 1,000
items and 64 MiB of estimated JSON payload bytes, with a 1 MiB per-item limit.
Override these with `KORA_ASYNC_HOOK_QUEUE_SIZE`,
`KORA_ASYNC_HOOK_MAX_QUEUED_BYTES`, and
`KORA_ASYNC_HOOK_MAX_EVENT_BYTES`; startup rejects nonpositive values and an
event cap larger than the aggregate cap.
The estimate is conservative and excludes Go allocator/object overhead; budget
an additional maximum-sized active item while the consumer executes. Oversized
submissions and queue saturation return errors so the originating operation
can report the failure. The queue snapshot API exposes current bytes, item
count, configured limits, and rejected item count for diagnostics.

Each tenant event fan-out permits at most 128 listeners. `Subscribe` returns a
capacity error when full; the legacy `AddListener` adapter closes an excess
channel and increments the dropped-event diagnostic. Analytics channels cap
their configured capacity at 10,000 items and reject encoded events above 1 MiB
by default before writing them to the WAL. Set
`KORA_ANALYTICS_MAX_EVENT_BYTES` to a value from 1 byte through 16 MiB to change
the event ceiling. The in-memory per-tenant wake-up channel also has a
conservative 64 MiB serialized-payload budget, configurable with
`KORA_ANALYTICS_MAX_QUEUED_BYTES` (positive values through 1 GiB). Its effective
slot count is the lower of the configured channel item count and
`floor(queue-byte-budget / event-byte-limit)`. If the byte budget is lower than
the event limit, the effective event limit is lowered to the queue budget so at
least one event can fit. Metrics expose queued items, configured item capacity,
and a conservative serialized-payload byte upper bound. The wrapped bus exposes
its event ceiling to MultiBus. Each subscriber queue is limited to a conservative
16 MiB payload bound, so its event capacity is derived from the configured event
ceiling (16 events at the 1 MiB default, one at the 16 MiB maximum). Local
realtime queues use the same 16 MiB bound. Listener count remains capped at 128
per tenant, and metrics aggregate listener count, occupancy, capacity, and
payload upper bounds across resident tenants. These estimates exclude Go object
overhead. `KORA_ANALYTICS_MAX_SUBSCRIBERS` also caps listener registrations
across the Engine process, so idle channel allocation is bounded independently
of tenant residency. The process-wide byte admission reserves maximum event
payload bytes only while events occupy queues; it does not claim to measure Go
channel/runtime overhead. The `NewChannelBusWithMaxEventBytes` constructor
remains source-compatible and uses the default byte budget. These bounds do not
include memory already owned by a caller constructing an event.
OTLP gauges report a conservative upper bound on the age of nonempty in-memory
analytics wake-up and subscriber queues. Durable WAL metrics report pending
state, bytes retained by resident WAL files, oldest known persisted-record
age, and separate uncertainty indicators when file size or timestamp
inspection is incomplete. Resident buses update byte counts after durable
writes and refresh them after acknowledgment. Cold-tenant WAL metrics continue
to report pending state and age; the byte gauge covers resident tenants only.
WAL byte counts are observations, not a disk quota or pruning permission.
WAL lines add the private `_kora_wal_written_at` field, which existing
`ChangeEvent` decoders ignore. An oldest record without it remains replayable and sets
`age_unknown` until that record is acknowledged. This age signal does not define a safe pruning
horizon: coordinated WAL, receipt, outbox, broker-redelivery, and backup-restore
retention remains open G3 work.

Local realtime streams use byte-derived listener queues and close/remove the
listener on overflow. NATS realtime subscriptions use an eight-message
application queue plus an eight-message/8 MiB NATS client pending limit; a
message over 1 MiB or any client-side dropped message closes the stream so the
consumer reconnects and refetches authorized state. The two NATS stages together
can retain approximately 16 MiB of payload per active stream, plus one message
being processed and Go object overhead. Local queues have a 16 MiB serialized
payload ceiling each; their event count falls as the configured event maximum
rises. Engine also caps concurrent
SSE/WebSocket handlers process-wide at 128 by default. Set
`KORA_REALTIME_MAX_STREAMS` to a positive value through 4,096 to change the
limit. Admission fails immediately with HTTP 503 and `Retry-After: 1`; clients
should reconnect with backoff and refetch. This is a per-process limit, so
replica count multiplies total deployment stream capacity. Metrics expose active
streams, configured capacity, and rejected admissions without tenant labels.
Local realtime listener occupancy is included in the aggregate analytics
listener queue metrics, including a conservative enqueue-age upper bound. NATS
realtime metrics report active subscriptions, NATS client pending message/byte
occupancy, application channel occupancy with a conservative payload-byte
upper bound, configured queue capacity, a conservative upper bound on age in
the Engine application channel, and cumulative client-side drops. The age is
tracked from local enqueue and can overstate after partial drains; it resets
once an empty queue is observed. NATS client-side pending age is not available
from the current client API. These are process-local values. The client drop count includes only drops the NATS
library can report; its subscription API notes that the count may be invalid
after the server declares the connection a slow consumer. Any deployment-wide
stream allocation policy remains open G3 work.

When OTLP metrics are enabled, Engine exports low-cardinality resource gauges
for aggregate endpoint connection capacity/use/waits/rejections, `database/sql`
open/in-use/idle/max-open pool counts and wait counts/durations across the
platform and resident tenant pools, active-work
capacity/use/waits/rejections, aggregate SQL driver operation counts and call
durations, and recognized lock-timeout/deadlock/busy errors, active and retiring runtime counts, async-hook
queue occupancy/capacity/rejections, tenant-scoped script activity/waiters,
analytics queue item, conservative payload, and in-memory age upper bounds, realtime stream capacity
and rejections, shared session-cache capacity, goroutines, heap
allocation/system bytes, current RSS from `/proc/self/statm` on Linux (zero on
platforms without that sampler), and analytics drop counters for payload
rejections, WAL write failures, subscriber queue overflows, and rejected
subscriber registrations across resident tenants. Semantic analytics result
cache metrics report entries, accounted bytes/capacity, evictions and rejected
cache insertions. The metrics omit tenant IDs, actor IDs, and endpoint
names. SQL operation metrics are process-local sums over endpoint connectors, have no SQL-text labels, and measure driver-call time until an initial result/row handle is returned (not row iteration). Lock contention recognizes MySQL lock wait/deadlock/nowait codes, PostgreSQL deadlock/lock-not-available SQL states, and SQLite busy/locked errors. It does not sample live server lock queues or count calls that fall back through a driver's prepared-statement path; full lock observability remains incomplete. Analytics listener queue count/occupancy, conservative byte capacity, and in-memory enqueue-age upper bounds are included. Durable analytics WAL pending state and oldest known persisted-record age are included; legacy/corrupt timestamp uncertainty is exposed separately. Resident tenant scheduler runtimes also export process-local durable-occurrence active, claim, claim-failure, lease-renewal-failure, fenced-lease-loss, uncertain-outcome, completion-failure, and backlog-sample-failure metrics. These are aggregated without tenant/job labels; a lost fence is reported separately from a transient renewal error and from an outcome the worker successfully marked uncertain.

The scheduler also refreshes the count and oldest scheduled time of materialized
pending occurrences every 30 seconds under tenant work admission. The query uses
a `(tenant_id, state, scheduled_at)` index and is cached for metric collection;
failed samples are counted, and OTel collection performs no SQL. This covers
resident schedulers only; the durable-work scanner remains responsible for
finding due work for unloaded tenants.

Resident analytics rebuild runners report active jobs, claims, claim failures,
active-work deferrals, execution failures, lease-renewal failures, fenced lease
loss, and completion/release failures through low-cardinality process metrics.
These describe loaded workers only; each tenant's durable rebuild-job table
remains the backlog authority and is discoverable when its runtime is unloaded.

Resident outbox publishers refresh cached pending, failed, and publishing lower-bound samples
capped at 256 timestamps per state, plus the oldest sampled unpublished-event age
every 30 seconds under endpoint-shared work admission. A saturation gauge signals
that a state may contain at least the sample cap. OTel collection reads the cache and does not issue SQL. These
gauges cover resident outbox-enabled tenants only; the durable-work scanner
reports cold tenants with due work but does not count their full outbox backlog.
The cached `kora.outbox.lease_lost` counter separately records publisher state
transitions rejected because a newer owner fenced the claim. The gauges expose
pressure and retry age; they do not bound durable history or authorize pruning.

Each `SessionManager` retains at most 8 MiB or 8,192 session entries, whichever
is reached first. It evicts least-recently-used entries under byte/count
pressure and removes expired or stale entries on a 30-second cache sweep;
database session cleanup remains on its separate five-minute cadence. An
oversized user snapshot still authenticates the current request but is not
cached. The exported cache occupancy/capacity metrics aggregate resident
tenant managers without tenant labels. Engine also applies one process-wide
256 MiB retained-snapshot budget across its process and tenant managers. Set
`KORA_SESSION_CACHE_MAX_BYTES` from 1 byte through 4 GiB to change it. When the
shared budget is full, new cache entries are skipped while the current request
still authenticates from its database result; existing entries are released on
manager eviction or close. Compatibility callers that construct independent
session managers outside Engine retain only the per-manager cap.

The process-wide semantic analytics query cache retains serialized result
bytes, tenant/query key bytes, and a fixed per-entry accounting allowance. It
admits at most 256 entries and 32 MiB total, with a 1 MiB maximum accounted
size for one entry. Oversized results are returned to the current caller
without caching; least-recently-used entries are evicted under count or byte
pressure. The cache occupancy/capacity and eviction/rejection counters are
included in Engine resource metrics. These byte figures bound retained
serialized payload and key data; Go map/list allocator overhead is additionally
bounded by the entry cap and is not an exact heap measurement.
The platform console-session store is separate and caps live sessions at 8,192;
when full, login first reclaims expired entries and returns a temporary capacity
error if all retained sessions remain live.

The process script runner also enforces tenant-scoped concurrency: by default,
two active scripts and four waiters per tenant, a one-second admission wait,
five-second maximum execution timeout, and at most 256 tenants with active or
waiting executions. Configure `KORA_SCRIPT_MAX_CONCURRENCY`,
`KORA_SCRIPT_MAX_WAITERS`, `KORA_SCRIPT_MAX_ACTIVE_TENANTS`,
`KORA_SCRIPT_WAIT_MS`, and `KORA_SCRIPT_TIMEOUT_MS`. This is an execution
concurrency/time bound; Goja's configured `MaxRAM` is still advisory and is not
a per-tenant heap limit. No `KORA_SCRIPTS_MAX_RAM` environment setting is
implemented, so it is omitted from the active environment reference.

Uploads are excluded from tenant database-work admission because they can be
long-running and use separate storage. Engine applies a process-wide weighted
upload budget instead: four active uploads and 512 MiB of conservative
processing reservation by default. Set `KORA_UPLOAD_MAX_ACTIVE` (1–256) and
`KORA_UPLOAD_ACTIVE_BUDGET_BYTES` (1 byte–4 GiB). Each upload reserves three
times its request-body limit to cover multipart parsing, decoded file bytes,
and image-processing copies; the hard per-upload body limit is 1 GiB, with a
50 MiB default from `KORA_MAX_UPLOAD_MB`. Requests that exceed the active count
or remaining reservation budget fail immediately with HTTP 503 and a retry
hint. The byte metric is a planning bound, not a measurement of allocator
overhead; filesystem temp storage may also be used during multipart parsing.
