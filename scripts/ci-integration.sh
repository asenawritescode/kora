#!/usr/bin/env bash
# Runs the integration-tagged Go suites against one SQL dialect.
#
# Isolation: every Go package gets freshly created databases of its own (one per
# DSN variable the package's tests read), so a package never sees tables left by
# another and a rerun starts clean. Variables are discovered from the tests
# (KORA_*_MYSQL_DSN / KORA_*_POSTGRES_DSN), so a new suite needs no change here.
# Tests that need a database per test create it from their DSN with
# internal/disposabledb.
#
# Usage: scripts/ci-integration.sh mysql|postgres
# Server defaults match the CI service containers; override for a local run:
#   KORA_IT_MYSQL_HOST/PORT, KORA_IT_POSTGRES_HOST/PORT, MYSQL_ROOT_PASSWORD,
#   PGUSER/PGPASSWORD.
# Every integration test must pass: there is no skip list. A test that cannot run
# in CI is fixed, or skipped in its own source with an issue link.
set -uo pipefail

dialect="${1:?usage: ci-integration.sh mysql|postgres}"

mysql_host="${KORA_IT_MYSQL_HOST:-127.0.0.1}"
mysql_port="${KORA_IT_MYSQL_PORT:-3306}"
pg_host="${KORA_IT_POSTGRES_HOST:-127.0.0.1}"
pg_port="${KORA_IT_POSTGRES_PORT:-5432}"
mysql_opts="parseTime=true&multiStatements=true"
mysql_root=(mysql -h "$mysql_host" -P "$mysql_port" -uroot -p"${MYSQL_ROOT_PASSWORD:-}")

db_name() { echo "it_$(echo "$1" | tr 'A-Z' 'a-z' | sed 's/^kora_//; s/_dsn$//')"; }
mysql_dsn() { echo "root:${MYSQL_ROOT_PASSWORD}@tcp(${mysql_host}:${mysql_port})/$1?${mysql_opts}"; }
postgres_dsn() { echo "postgres://${PGUSER}:${PGPASSWORD}@${pg_host}:${pg_port}/$1?sslmode=disable"; }

# DSN variables read by the tests of one package directory.
package_vars() {
  local pattern
  case "$dialect" in
    mysql) pattern='"(KORA_TEST_DSN|KORA_REQUEST_ISOLATION_DISPOSABLE_DSN|KORA_[A-Z0-9_]+_MYSQL_DSN)"' ;;
    postgres) pattern='"KORA_[A-Z0-9_]+_POSTGRES_DSN"' ;;
  esac
  grep -hoE "$pattern" "$1"/*_test.go 2>/dev/null | tr -d '"' | sort -u
}

recreate_database() {
  case "$dialect" in
    mysql)
      "${mysql_root[@]}" -e "DROP DATABASE IF EXISTS \`$1\`; CREATE DATABASE \`$1\` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci" ;;
    postgres)
      psql -h "$pg_host" -p "$pg_port" -U "$PGUSER" -d postgres -q -v ON_ERROR_STOP=1 \
        -c "DROP DATABASE IF EXISTS \"$1\" WITH (FORCE)" -c "CREATE DATABASE \"$1\"" ;;
  esac
}

# Exports the DSN variables for one package and recreates their databases.
prepare_package() {
  local v db
  for v in $(package_vars "$1"); do
    db=$(db_name "$v")
    if ! recreate_database "$db" >/dev/null; then
      echo "cannot create database $db for $v" >&2
      return 1
    fi
    case "$dialect" in
      mysql) export "$v=$(mysql_dsn "$db")" ;;
      postgres) export "$v=$(postgres_dsn "$db")" ;;
    esac
  done
}

case "$dialect" in
  mysql) : "${MYSQL_ROOT_PASSWORD:?set MYSQL_ROOT_PASSWORD}" ;;
  postgres) : "${PGUSER:?set PGUSER}" "${PGPASSWORD:?set PGPASSWORD}" ;;
  *) echo "unknown dialect $dialect" >&2; exit 2 ;;
esac

failed=()
while read -r pkg dir <&3; do
  if ! prepare_package "$dir"; then
    failed+=("$pkg (database setup)")
    continue
  fi
  # stdin is the package list on fd 3, but keep tools from reading fd 0 as well.
  if ! go test -tags integration -count=1 -timeout 20m "$pkg" </dev/null; then
    failed+=("$pkg")
  fi
done 3< <(go list -tags integration -f '{{.ImportPath}} {{.Dir}}' ./...)

if [ "${#failed[@]}" -gt 0 ]; then
  printf 'Failed packages:\n'; printf '  %s\n' "${failed[@]}"
  exit 1
fi
