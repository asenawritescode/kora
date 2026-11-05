#!/usr/bin/env bash
# Runs the integration-tagged Go suites against one SQL dialect.
# Every package starts with freshly created databases, because several suites
# share one DSN variable across packages and leave tables behind.
#
# Usage: scripts/ci-integration.sh mysql|postgres
# Tests listed in scripts/ci-integration-known-failures/<dialect>.txt are skipped.
set -uo pipefail

dialect="${1:?usage: ci-integration.sh mysql|postgres}"

# Suites whose DSN points at the server; they create and drop their own databases.
mysql_server_vars=(
  KORA_TEST_DSN KORA_ANALYTICS_DISPOSABLE_MYSQL_DSN KORA_OUTBOX_DISPOSABLE_MYSQL_DSN
  KORA_API_LIVE_MYSQL_DSN KORA_REQUEST_ISOLATION_DISPOSABLE_DSN
)
# Suites that expect a database of their own.
mysql_db_vars=(
  KORA_AI_INVENTORY_DISPOSABLE_MYSQL_DSN KORA_AI_LIVE_MYSQL_DSN KORA_AI_MUTATION_MYSQL_DSN
  KORA_ASYNC_HOOK_DISPOSABLE_MYSQL_DSN KORA_CONFIG_DISPOSABLE_MYSQL_DSN KORA_CRM_DISPOSABLE_MYSQL_DSN
  KORA_DBACCESS_DISPOSABLE_MYSQL_DSN KORA_DURABLE_SCAN_DISPOSABLE_MYSQL_DSN
  KORA_ENGINE_HOOK_DISPOSABLE_MYSQL_DSN KORA_ENGINE_MODULE_COMMAND_DISPOSABLE_MYSQL_DSN
  KORA_ENGINE_WORKFLOW_DISPOSABLE_MYSQL_DSN KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN
  KORA_MANAGEMENT_PROVISION_DISPOSABLE_MYSQL_DSN KORA_MCP_READ_MYSQL_DSN
  KORA_PROCESS_DISPOSABLE_MYSQL_DSN KORA_PUBLISHED_QUERY_DISPOSABLE_MYSQL_DSN
  KORA_RECON_DISPOSABLE_MYSQL_DSN KORA_SCHEDULER_DISPOSABLE_MYSQL_DSN
  KORA_SITE_CONFIG_VERSION_DISPOSABLE_MYSQL_DSN KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN
  KORA_TODO_DISPOSABLE_MYSQL_DSN KORA_WORKFLOW_DISPOSABLE_MYSQL_DSN
)
postgres_db_vars=(
  KORA_AI_INVENTORY_DISPOSABLE_POSTGRES_DSN KORA_AI_MUTATION_POSTGRES_DSN
  KORA_ANALYTICS_DISPOSABLE_POSTGRES_DSN KORA_API_DISPOSABLE_POSTGRES_DSN KORA_API_LIVE_POSTGRES_DSN
  KORA_ASYNC_HOOK_DISPOSABLE_POSTGRES_DSN KORA_CONFIG_DISPOSABLE_POSTGRES_DSN
  KORA_CONFIG_REVISION_DISPOSABLE_POSTGRES_DSN KORA_DBACCESS_DISPOSABLE_POSTGRES_DSN
  KORA_DURABLE_SCAN_DISPOSABLE_POSTGRES_DSN KORA_ENGINE_HOOK_DISPOSABLE_POSTGRES_DSN
  KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN KORA_MANAGEMENT_PROVISION_DISPOSABLE_POSTGRES_DSN
  KORA_NOTIFICATIONS_DISPOSABLE_POSTGRES_DSN KORA_OUTBOX_DISPOSABLE_POSTGRES_DSN
  KORA_PROCESS_DISPOSABLE_POSTGRES_DSN KORA_RECON_DISPOSABLE_POSTGRES_DSN
  KORA_REQUESTS_DISPOSABLE_POSTGRES_DSN KORA_SCHEDULER_DISPOSABLE_POSTGRES_DSN
  KORA_SECRET_LIVE_POSTGRES_DSN KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN
  KORA_SITE_DIRECTORY_SUMMARY_POSTGRES_DSN KORA_WORKFLOW_DISPOSABLE_POSTGRES_DSN
)

db_name() { echo "it_$(echo "$1" | tr 'A-Z' 'a-z' | sed 's/^kora_//; s/_dsn$//')"; }

mysql_root=(mysql -h 127.0.0.1 -P 3306 -uroot -p"${MYSQL_ROOT_PASSWORD:-}")
mysql_opts="parseTime=true&multiStatements=true"

reset_databases() {
  case "$dialect" in
    mysql)
      for v in "${mysql_db_vars[@]}"; do
        db=$(db_name "$v")
        "${mysql_root[@]}" -e "DROP DATABASE IF EXISTS \`$db\`; CREATE DATABASE \`$db\` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci" 2>/dev/null
      done ;;
    postgres)
      for v in "${postgres_db_vars[@]}"; do
        db=$(db_name "$v")
        psql -h 127.0.0.1 -U "$PGUSER" -d postgres -q -v ON_ERROR_STOP=1 \
          -c "DROP DATABASE IF EXISTS \"$db\" WITH (FORCE)" -c "CREATE DATABASE \"$db\""
      done ;;
  esac
}

case "$dialect" in
  mysql)
    for v in "${mysql_server_vars[@]}"; do export "$v=root:${MYSQL_ROOT_PASSWORD}@tcp(127.0.0.1:3306)/?${mysql_opts}"; done
    for v in "${mysql_db_vars[@]}"; do export "$v=root:${MYSQL_ROOT_PASSWORD}@tcp(127.0.0.1:3306)/$(db_name "$v")?${mysql_opts}"; done ;;
  postgres)
    for v in "${postgres_db_vars[@]}"; do export "$v=postgres://${PGUSER}:${PGPASSWORD}@127.0.0.1:5432/$(db_name "$v")?sslmode=disable"; done ;;
  *) echo "unknown dialect $dialect" >&2; exit 2 ;;
esac

known_failures="$(dirname "$0")/ci-integration-known-failures/${dialect}.txt"
skip_args=()
if [ -f "$known_failures" ]; then
  names=$(sed 's/#.*//' "$known_failures" | awk 'NF {print $1}' | paste -sd'|')
  if [ -n "$names" ]; then
    skip_args=(-skip "^(${names})\$")
    echo "Skipping $(echo "$names" | tr '|' '\n' | wc -l) known ${dialect} failures listed in ${known_failures}"
  fi
fi

failed=()
for pkg in $(go list -tags integration ./...); do
  reset_databases
  if ! go test -tags integration -count=1 -timeout 20m "${skip_args[@]}" "$pkg"; then
    failed+=("$pkg")
  fi
done

if [ "${#failed[@]}" -gt 0 ]; then
  printf 'Failed packages:\n'; printf '  %s\n' "${failed[@]}"
  exit 1
fi
