# Local self-hosted Compose example

This example runs one Engine, the standalone `kora-ui` workspace, and a
dedicated MySQL 8.4.11 database. It does not require Cloud, NATS, Redis,
Kubernetes, or a pooler. The workspace checkout is expected at
`../kora-studio-mvp`; set `KORA_UI_SOURCE` in `.env` when the checkouts are
elsewhere.

The Engine stores local attachment blobs and metadata below `/sites`, backed by
the `engine_sites` named volume. The MySQL service is private to the Compose network. The UI binds to
`127.0.0.1:8080` and should sit behind a TLS terminator using `nginx.conf` or an
equivalent same-origin proxy. Keep tenant routes, cookies, CSRF, and realtime
traffic on that origin. The init grant gives the Kora account access to all
schemas because this example uses one dedicated database server for platform
and tenant databases. Do not reuse that SQL server for unrelated applications.

Cloud is optional. With `KORA_CLOUD_URL` unset, the workspace keeps optional
Cloud calls on its own origin and the UI proxy returns 404 for `/api/cloud`;
Engine-backed tenant operations continue to work. To enable optional Cloud
features, set `KORA_CLOUD_URL` to the operator's Cloud API origin reachable on
the private Compose network (for example `http://kora-cloud:8081`) in the
deployment `.env`. The UI proxies those routes on the existing browser origin,
preserving cookies and CSRF behavior. Do not point this setting at a public
managed Cloud service unless that data flow is an intentional operator choice.

To let Cloud provision tenants through the Engine management API, also set
`KORA_ENGINE_MANAGEMENT_TOKEN`, `KORA_ENGINE_MANAGEMENT_CALLER_ID`,
`KORA_ENGINE_DEPLOYMENT_ID`, and
`KORA_ENGINE_MANAGEMENT_DATABASE_ALLOCATION_REF` in the Engine `.env`.
Configure Cloud with the same Engine base URL and token, enable
`KORA_CLOUD_ENGINE_MANAGEMENT_PROVISIONING`, and set its
`KORA_CLOUD_ENGINE_MANAGEMENT_DEPLOYMENT` and
`KORA_CLOUD_ENGINE_MANAGEMENT_DATABASE_ALLOCATION_REF` to the exact same
deployment and allocation references. Use the Engine origin as the management
URL (for example `http://engine:8000` on a private Compose network), not a
tenant `/s/<alias>` URL. The Engine only registers management routes when its
caller identity and management credentials are configured; tenant provisioning
is advertised only when the deployment and database-allocation reference are
also configured. Verify `GET /api/management/v2/capabilities` with the
management bearer token before enabling Cloud provisioning. Engine inspection
routes require the management token and deployment ID; `/api/management/v2/capabilities`
and durable operation routes are added only when the stable caller ID is also
configured. Tenant provisioning is advertised only when the database-allocation
reference is configured. Keep the deployment ID aligned with existing directory
assignments (`default` for legacy blank assignments) unless those assignments
have been deliberately migrated. A missing capability route is a
configuration/version mismatch and should be fixed at the Engine deployment;
do not fall back to Cloud opening tenant databases.

Cloud's managed channel credential rotation uses a separate narrow Engine
credential. Set `KORA_ENGINE_PROVISIONING_TOKEN` on Engine and the exact same
secret as `KORA_CLOUD_ENGINE_PROVISIONING_TOKEN` on Cloud. Keep it distinct
from the Engine management API token and the issued tenant channel credential.
The Engine accepts it only on managed-channel rotate/revoke and site-identity
routes; if the values do not match, Cloud receives HTTP 401 when issuing a
managed channel credential.

For path-routed tenants, set `KORA_PUBLIC_APP_BASE_URL` to the public workspace
origin in `.env` (for example `https://app.example.com`). Engine uses it to
create sign-in links at `/s/<tenant>/workspace/auth/login`; it never derives
the app origin from a request or tenant alias. Leave it empty only when each
tenant has its own directly routed hostname. Loopback HTTP is accepted for
local development; non-loopback origins require HTTPS.

Configure `KORA_SMTP_HOST`, `KORA_SMTP_PORT`, `KORA_SMTP_FROM`, and (when
required by the provider) `KORA_SMTP_USERNAME` and `KORA_SMTP_PASSWORD` to
deliver sign-in and administrator-invitation links. `KORA_SMTP_TLS_MODE` accepts
`auto`, `implicit`, `starttls`, or `plain`. Without an SMTP host, Engine does
not deliver email; magic-link requests for existing accounts now return an
error instead of reporting a successful send. For local testing, Mailpit can
be run on the Compose network as `mailpit:1025`, or reached from the Engine
container using a host gateway address. Do not use `plain` with a remote
provider.

Start with operator-generated secrets:

```sh
cp deploy/selfhost/.env.example deploy/selfhost/.env
# Edit .env and replace every example secret before starting services.
docker compose -f deploy/selfhost/compose.yaml --env-file deploy/selfhost/.env config --quiet
docker compose -f deploy/selfhost/compose.yaml --env-file deploy/selfhost/.env up -d --build
curl --fail http://127.0.0.1:8080/api/ping
docker compose -f deploy/selfhost/compose.yaml --env-file deploy/selfhost/.env \
  exec -T engine curl --fail http://127.0.0.1:8000/readyz
```

`/api/ping` through the UI proxy is liveness. `/readyz` on the Engine
container checks the canonical platform-directory DB with a two-second
deadline; it does not certify every tenant DB, worker queue, or external
service. Verify tenant operations and durable work separately.

For maintenance, stop incoming traffic first, then stop the Engine while
MySQL remains available. The Compose Engine service allows 65 seconds for its
bounded shutdown and possible drain retry; do not stop MySQL first or replace
this with `docker kill`. Wait for `docker compose stop engine` to return and
inspect Engine logs for shutdown errors before database maintenance. `docker
compose down` respects dependency order and the Engine grace period.

The Engine management inspection route is disabled unless
`KORA_ENGINE_MANAGEMENT_TOKEN` and `KORA_ENGINE_DEPLOYMENT_ID` are set. Keep
that bearer secret separate from provisioning and managed-channel credentials.
The API exposes credential-free tenant descriptors. Setting
`KORA_ENGINE_MANAGEMENT_CALLER_ID` additionally enables durable operation
capabilities and submission/polling routes for configured Engine-owned
operations, including active/suspended state transitions;
the caller value must be a stable Engine principal ID. That state executor
revision-fences the directory update and converges the local tenant runtime.
Cloud sign-in handoff (including accepting a member invitation) needs both
`KORA_ENGINE_CLOUD_IDENTITY_URL` (Cloud's private
`/internal/identity/handoff/redeem` endpoint, for local development
`http://127.0.0.1:8081/internal/identity/handoff/redeem`) and
`KORA_ENGINE_CLOUD_IDENTITY_TOKEN` (the Cloud engine provisioning token). With
neither set, the Engine logs one startup WARN naming both variables and
`POST /api/auth/cloud/handoff` answers `503` with code
`auth.cloud_handoff_unconfigured`; with only one set, startup fails. Values are
never logged. The local `scripts/restart-server.sh` forwards both from `.env.local`.

For deployment ingress, set `KORA_ENGINE_LOCAL_DEPLOYMENT_ID` and
`KORA_ENGINE_DEPLOYMENT_URLS` as comma-separated
`deployment-id=https://direct-engine-origin` pairs. Origins must point directly
to Engine, not back to a public ingress. The legacy `KORA_ENGINE_CELL_ID` and
`KORA_ENGINE_CELL_URLS` values remain adapters; if both names for the local ID
or endpoint map are set, they must agree. Leave these values unset for the
supported single-Engine self-hosted deployment when management deployment ID
is also unset. If `KORA_ENGINE_DEPLOYMENT_ID` is configured, it becomes the
local placement fallback; make sure the tenant directory assignments match it
(legacy blank assignments belong to `default`).
For a self-hosted installation with multiple Engine deployments, the operator
may run the separately packaged ingress process without Cloud's control-plane
services. Follow the [deployment ingress runbook](https://github.com/asenawritescode/kora-cloud/blob/main/docs/deployment-ingress.md)
for direct-origin configuration, health probes, tenant-path validation, and
assignment rollback. One Engine remains the supported default and needs no
ingress.
For matching MySQL or PostgreSQL platform/tenant databases, tenant provisioning
is enabled only when
`KORA_ENGINE_MANAGEMENT_DATABASE_ALLOCATION_REF` names the single database
allocation assigned to this Engine deployment. Engine accepts only that exact
opaque reference and the configured deployment ID, and resolves connection
settings from its own database configuration. It creates the passwordless
bootstrap administrator but does not deliver an enrollment link. A
`recovery_required` package-free provision can be retried through the audited
`reconcile-tenant-provision` operation route after verifying tenant identity
and allocation. Package activation is a separate Engine operation: configure
both `KORA_ENGINE_MANAGEMENT_ARTIFACT_URL` and
`KORA_ENGINE_MANAGEMENT_ARTIFACT_TOKEN` to enable it. The provider must be
Cloud or another trusted service implementing the immutable artifact contract;
use HTTPS outside loopback and keep this bearer token distinct from the
management API token. Cloud also requires its package-management opt-in and
the matching dedicated artifact token. Package activation is not yet a
production release claim; complete the remaining transactional recovery and
consumer compatibility gates first. Tenant deletion remains unavailable until
its recovery gates pass. LibSQL provisioning remains disabled until a trusted
adapter creates separate tenant databases. Leave the caller ID unset when no
management operations are needed.
Resource limits use a static per-replica allocation; recalculate the values
before scaling. The `.env.example` exposes the database connection/work caps,
resident-tenant and activation limits, analytics and async-hook byte/count
budgets, script execution limits, realtime stream limit, session-cache budget,
and upload admission budget. Analytics has separate per-tenant wake-up queue
and aggregate process queue byte settings. `KORA_ANALYTICS_MAX_SUBSCRIBERS`
caps listener channels across one Engine process. These limits are process-local; update the
per-replica database allocation and process resource budget when scaling. The
compose example does not enable poolers. PgBouncer and
ProxySQL samples are unvalidated configuration starting points only; run the
specification's compatibility tests before using a pooled runtime endpoint,
and keep session-dependent administrative work on its direct/session pool.

The container tags are pinned in `compose.yaml` and the standalone UI's
existing `Dockerfile`. The target production release should replace local
builds with reviewed release digests after the consumer release gates pass.
This is an example deployment, not evidence of production readiness. See
[`backup.md`](backup.md) and [`restore.md`](restore.md) before storing data.

On Engine upgrade, pending analytics WALs move automatically from recognized
route-name directories to stable tenant-ID directories before tenant workers
start. Keep the analytics WAL volume mounted and backed up during the upgrade.
If startup reports an unassigned legacy path or both old and new paths contain
pending events, leave the files intact and resolve their tenant ownership before
retrying; do not delete or combine WAL files by hand. This migration does not
bound WAL disk growth or make the coordinated replay-retention gate safe.

## Consumer release order

1. Verify the database backup and migration plan. Deploy the Engine build that
   converts stored document lifecycle configurations to Engine v2 before
   activating each tenant. The migration validates converted definitions and
   fails tenant startup if it finds an unsupported transition hook or invalid
   policy; fix the reported configuration and retry. The retired lifecycle
   runtime is not kept as a fallback, and legacy execution history is not
   migrated.
2. Confirm Engine `/readyz` and verify each tenant activates with its migrated
   Engine v2 definitions before moving traffic.
3. Release the standalone UI against the Engine v2 contract. Keep Automations
   controls hidden unless Engine advertises the supported implementation.
4. Release Cloud management consumers only after the typed Engine management
   API/client and durable operation recovery are deployed. Cloud is optional
   for self-hosted Engine operation.
5. Handle cell-routing compatibility through its separate migration and
   cutover gate; it is unrelated to document lifecycle automation.

Rollback by disabling new starts, keeping workers needed by active persisted
work, and returning to an Engine/UI pair that can read the written schema.
Retain additive tables and columns. Never downgrade across a persisted-format
boundary or restore over live data as a routine application rollback.
