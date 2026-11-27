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
# Usage: scripts/ci-integration.sh [--list] mysql|postgres
#   --list prints the packages this run would execute and the predicted seconds,
#   without touching any database.
# Server defaults match the CI service containers; override for a local run:
#   KORA_IT_MYSQL_HOST/PORT, KORA_IT_POSTGRES_HOST/PORT, MYSQL_ROOT_PASSWORD,
#   PGUSER/PGPASSWORD.
# Every integration test must pass: there is no skip list. A test that cannot run
# in CI is fixed, or skipped in its own source with an issue link.
#
# Sharding: KORA_IT_SHARD=i/n (1-based i) runs one of n shards. Packages are split
# by measured cost (scripts/ci-integration-weights.txt, seconds per package, the
# slower dialect's time). Packages are taken heaviest first and each goes to the
# currently lightest shard, lowest index on a tie. A package missing from the weights
# file counts as 10 s. Unset: one shard holding every package.
set -uo pipefail

list_only=""
if [ "${1:-}" = "--list" ]; then
  list_only=1
  shift
fi
dialect="${1:?usage: ci-integration.sh [--list] mysql|postgres}"

case "$dialect" in
  mysql|postgres) ;;
  *) echo "unknown dialect $dialect" >&2; exit 2 ;;
esac

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
weights_file="$script_dir/ci-integration-weights.txt"
if [ ! -r "$weights_file" ]; then
  echo "cannot read package weights $weights_file" >&2
  exit 2
fi

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

# plan_shard SHARD COUNT: reads "<import path> <dir>" lines sorted by path and prints
# the lines of shard SHARD (1-based) of COUNT, in path order. The predicted seconds
# go to stderr before the list.
plan_shard() {
  awk -v shard="$1" -v count="$2" -v weights="$weights_file" '
    BEGIN {
      while ((getline line < weights) > 0) { split(line, f, " "); w[f[1]] = f[2] }
      close(weights)
    }
    { n++; path[n] = $1; dir[n] = $2; wt[n] = ($1 in w) ? w[$1] : 10 }
    END {
      # Heaviest first; equal weights keep path order (insertion sort is stable).
      for (i = 1; i <= n; i++) ord[i] = i
      for (i = 2; i <= n; i++) {
        cur = ord[i]; j = i - 1
        while (j >= 1 && wt[ord[j]] < wt[cur]) { ord[j + 1] = ord[j]; j-- }
        ord[j + 1] = cur
      }
      for (s = 1; s <= count; s++) load[s] = 0
      for (i = 1; i <= n; i++) {
        k = ord[i]; best = 1
        for (s = 2; s <= count; s++) if (load[s] < load[best]) best = s
        owner[k] = best; load[best] += wt[k]
      }
      size = 0
      for (k = 1; k <= n; k++) if (owner[k] == shard) size++
      printf "shard %d/%d: %d packages, predicted %.1fs\n", shard, count, size, load[shard] > "/dev/stderr"
      for (k = 1; k <= n; k++) if (owner[k] == shard) print path[k], dir[k]
    }'
}

shard_index=1
shard_count=1
if [ -n "${KORA_IT_SHARD:-}" ]; then
  if [[ ! "$KORA_IT_SHARD" =~ ^([1-9][0-9]*)/([1-9][0-9]*)$ ]] || [ "${BASH_REMATCH[1]}" -gt "${BASH_REMATCH[2]}" ]; then
    echo "KORA_IT_SHARD must be i/n with 1 <= i <= n, got: $KORA_IT_SHARD" >&2
    exit 2
  fi
  shard_index=${BASH_REMATCH[1]}
  shard_count=${BASH_REMATCH[2]}
fi

if [ -z "$list_only" ]; then
  case "$dialect" in
    mysql) : "${MYSQL_ROOT_PASSWORD:?set MYSQL_ROOT_PASSWORD}" ;;
    postgres) : "${PGUSER:?set PGUSER}" "${PGPASSWORD:?set PGPASSWORD}" ;;
  esac
fi

packages=$(go list -tags integration -f '{{.ImportPath}} {{.Dir}}' ./...) || {
  echo "go list failed; no packages planned" >&2
  exit 2
}
plan=$(printf '%s\n' "$packages" | sort | plan_shard "$shard_index" "$shard_count") || exit 2

if [ -n "$list_only" ]; then
  printf '%s\n' "$plan" | cut -d' ' -f1
  exit 0
fi

failed=()
while read -r pkg dir; do
  [ -n "$pkg" ] || continue
  if ! prepare_package "$dir"; then
    failed+=("$pkg (database setup)")
    continue
  fi
  if ! go test -tags integration -count=1 -timeout 20m "$pkg" </dev/null; then
    failed+=("$pkg")
  fi
done <<<"$plan"

if [ "${#failed[@]}" -gt 0 ]; then
  printf 'Failed packages:\n'; printf '  %s\n' "${failed[@]}"
  exit 1
fi
