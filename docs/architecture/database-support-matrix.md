# Engine database support matrix

This matrix records what the current tree configures and tests. A dialect
implementation or an environment-gated integration test does not, by itself,
establish complete production support.

| Backend | Current role | Implementation evidence | Real-database evidence in this tree | Status |
|---|---|---|---|---|
| MySQL | Default and documented self-host backend | `db/dialect_mysql.go`; `go-sql-driver/mysql`; self-host Compose pins `mysql:8.4.11` | Engine CI and the self-host Compose file pin `mysql:8.4.11`. CI runs integration-tagged `internal/dbaccess`, `api`, `analytics`, `outbox`, `site`, `requests/sqlstore`, `workflow/sqlstore`, `notifications/sqlstore`, and `management/sqlstore` packages against its fresh MySQL service. A dedicated CI database runs the real lock-contention metric test through the budgeted connector. The API step enables `KORA_TEST_DSN` and `KORA_CONFIG_DISPOSABLE_MYSQL_DSN` for isolated API/kernel, config activation, and realtime authorization/replay tests; the other DB-specific disposable DSNs are also enabled. Cloud remains separately pinned to `mysql:8.4`, while Cloud's local disposable-stack guide still uses `mysql:8.0`; those Cloud versions are a G7 release-boundary follow-up, not evidence of the Engine CI matrix. API channel-handler live tests and site-directory live tests use separate opt-in DSNs such as `KORA_API_LIVE_MYSQL_DSN` and `KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN`; CI does not run those paths. Analytics exercises v2→v5 bootstrap, including the v4→v5 additive source-sequence receipt migration, rebuild cutoff/flush handoff, concurrent job claims, expired-lease takeover, and stale completion rejection. A fresh MySQL 8.4.11 bootstrap migration test upgrades old outbox and consumer-receipt tables, preserves existing events and receipts, adds the tenant-sequence singleton, and verifies source-sequence columns/indexes; transactional receipt integration verifies persisted sequence. The G7 revision-fenced site state transition test additionally passed on a fresh disposable MySQL 8.4.10 container. | Primary self-host path. CI exercises the listed database packages on the same MySQL release as the self-host example. Cross-repository Cloud version alignment, additional API/site-directory live paths, and the full G1–G10 acceptance matrix are not yet proven. |
| PostgreSQL | Implemented tenant/platform dialect | `db/dialect_postgres.go`; `github.com/lib/pq` | Real integration coverage exists for configstore, site directory/runtime, CLI startup with split platform/tenant backends, secrets, analytics, AI runs, kernel/API operations, and users. Tests use disposable DSNs including `KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN`, `KORA_API_LIVE_POSTGRES_DSN`, and package-specific variables. Analytics exercises the v4→v5 additive source-sequence receipt migration alongside rebuild cutoff/flush handoff, concurrent job claims, expired-lease takeover, stale completion rejection, and receipt sequence persistence. A fresh PostgreSQL 16 bootstrap migration test upgrades old outbox and consumer-receipt tables, preserves existing events and receipts, adds the tenant-sequence singleton, and verifies source-sequence columns/indexes; transactional receipt integration verifies persisted sequence. The G7 revision-fenced site lifecycle test also passed on a fresh disposable PostgreSQL 16 container. | Partial compatibility evidence. No PostgreSQL self-host deployment profile or full contract matrix is verified. |
| LibSQL | Implemented remote LibSQL dialect and transport | `db/dialect_libsql.go`; `github.com/tursodatabase/libsql-client-go` | Tests open the actual `libsql` `database/sql` driver and require disposable remote DSNs, including `KORA_REQUESTS_DISPOSABLE_LIBSQL_DSN`, `KORA_WORKFLOW_DISPOSABLE_LIBSQL_DSN`, `KORA_SITE_DIRECTORY_LIVE_LIBSQL_DSN`, and API/AI package variables. Actual-HTTP integration tests exercise the analytics v4→v5 and tenant bootstrap v18→v19 additive receipt-sequence migrations, preserve historical receipt rows with unknown sequence, and verify new sequenced receipt persistence. Other tests cover outbox bootstrap, source sequences, lease and workflow behavior. The G7 revision-fenced site lifecycle test also passed through a fresh disposable LibSQL HTTP server. | Partial transport and package evidence. No local SQLite driver is registered, and the full Engine persistence matrix is not verified. Single tenant only: all LibSQL tenants resolve to `DB_DSN` with unprefixed business tables, so activation refuses a second tenant on the same database. |

## Reproducible checks

These commands exercise deterministic unit and SQL-generation coverage without
external credentials:

```sh
go test ./db ./configstore ./site ./analytics ./requests/... ./workflow/... ./outbox -count=1
go test -race ./internal/dbaccess ./analytics ./requests/... ./workflow/... -count=1
go vet ./...
```

Real-database tests use the `integration` build tag and package-specific
environment variables. Run them only with disposable services and databases;
do not point these tests at production or user data. The variables ending in
`_DISPOSABLE_*_DSN` are explicit safety cues. Other `*_DSN` variables are not
assumed disposable and must be reviewed before execution. Tests that skip when
their DSN is absent provide no live-driver evidence for that run.

The primary local deployment profile is in [`deploy/selfhost`](../../deploy/selfhost/README.md)
and currently provisions MySQL 8.4.11. PgBouncer and ProxySQL samples remain
optional and unvalidated; no pooled production endpoint is recommended until
its compatibility suite passes.

## Known validation boundary

The current integration tests are distributed by package and use several
different opt-in DSN names. They are not one centrally managed, exhaustive
matrix. Current test results and skipped external checks are recorded in the
[implementation ledger](implementation-status.md). This matrix must be
expanded when a supported backend/version is declared or a complete CI
service matrix is added.
