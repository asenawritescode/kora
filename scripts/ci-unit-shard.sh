#!/usr/bin/env bash
# Print the Go packages of one unit-test shard, one per line.
#
# Packages are split greedily, heaviest first, onto the lightest shard (ties go to the lower
# index). Weights are measured seconds from scripts/ci-unit-weights.txt; packages not listed
# weigh 1. The split is deterministic, so every CI shard job computes the same partition.
#
# Usage: KORA_UNIT_SHARD=2/3 scripts/ci-unit-shard.sh
set -euo pipefail

spec="${KORA_UNIT_SHARD:?set KORA_UNIT_SHARD=i/n}"
index="${spec%/*}"
total="${spec#*/}"
if ! [[ "$index" =~ ^[0-9]+$ && "$total" =~ ^[0-9]+$ ]] || [ "$index" -lt 1 ] || [ "$index" -gt "$total" ]; then
  echo "KORA_UNIT_SHARD must be i/n with 1 <= i <= n, got: $spec" >&2
  exit 2
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
weights="$root/scripts/ci-unit-weights.txt"
cd "$root"

out="$(
  go list ./... | awk -v wfile="$weights" '
    FNR == NR { if ($0 !~ /^#/ && NF == 2) w[$1] = $2; next }
    { print (($1 in w) ? w[$1] : 1) "\t" $1 }
  ' "$weights" - | sort -t "$(printf '\t')" -k1,1gr -k2,2 | awk -F '\t' -v shard="$index" -v total="$total" '
    BEGIN { for (i = 1; i <= total; i++) load[i] = 0 }
    {
      best = 1
      for (i = 2; i <= total; i++) if (load[i] < load[best]) best = i
      load[best] += $1
      if (best == shard) print $2
    }
  ' | sort
)"

if [ -z "$out" ]; then
  echo "shard $spec has no packages" >&2
  exit 1
fi
printf '%s\n' "$out"
