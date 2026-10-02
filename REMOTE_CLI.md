# Remote Kora CLI

`kora remote` manages a Kora site through the same HTTP APIs used by other
clients. It does not connect to the remote database and it does not bypass the
site's role, DocType, or record permissions.

## Configure a profile

Create a least-privilege extension or delegated channel token on the target
site, then put it in an environment variable. Tokens are deliberately not
accepted as command-line flags or stored in the profile file, where they could
leak through shell history or backups.

```sh
export ACME_KORA_TOKEN='...'

kora remote profile set acme \
  --url https://kora.example.com \
  --site acme \
  --token-env ACME_KORA_TOKEN

kora remote profile use acme
kora remote status
```

For a site with its own domain, omit `--site`. For local development, HTTP is
allowed on localhost and loopback addresses. Other HTTP endpoints are rejected
unless `--allow-http` is explicitly configured.

Profiles are stored in the operating system's user config directory as
`kora/remote.json` with mode `0600`. They contain the URL, site, and token
environment-variable name only.

## Inspect schemas

```sh
kora remote schema list
kora remote schema get "Work Order"
```

Friendly DocType names are converted to stable dashed API identifiers, so this
request uses `/api/v1/system/doctype/work-order`, never a `%20` path. Existing
explicit underscore aliases remain valid for compatibility.

Delegated and extension credentials only see schemas for DocTypes included in
their permission grant.

## Manage records

```sh
kora remote records list "Work Order" --limit 20
kora remote records get "Work Order" WO-0001

kora remote records create "Work Order" \
  --data '{"title":"Inspect generator"}' \
  --idempotency-key agent-run-42-create-work-order

kora remote records update "Work Order" WO-0001 \
  --file update.json \
  --idempotency-key agent-run-42-update-work-order

kora remote records delete "Work Order" WO-0001 --yes
```

Use `--file -` to read a JSON object from standard input. Delete requires an
explicit `--yes`. Create and update use Kora's operation kernel for atomic
audit, idempotency, and validation. The server remains authoritative: a token
without the needed read, create, write, or delete permission receives `403`.

## Agent use

Give each agent its own short-lived, least-privilege token and environment
variable. Keep separate profiles for separate sites. Prefer idempotency keys for
retriable writes, and revoke the delegated session when the run ends.

This first remote surface intentionally excludes configuration activation,
console site deletion, secrets, and raw arbitrary HTTP calls. Those operations
need dedicated typed commands with confirmation and audit semantics rather than
a generic privilege tunnel.
