#!/usr/bin/env bash
set -euo pipefail

# Cross-repository Phase 0 gate. Each repository owns its own test process;
# this runner only composes the gates and never mutates application state.
ENGINE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLOUD_ROOT="${ENGINE_ROOT}-cloud"
STUDIO_ROOT="${ENGINE_ROOT}-studio-mvp"
WEBSITE_ROOT="${ENGINE_ROOT}-website"

for required in "$ENGINE_ROOT" "$CLOUD_ROOT" "$STUDIO_ROOT" "$WEBSITE_ROOT"; do
  if [[ ! -d "$required" ]]; then
    echo "acceptance: missing repository $required" >&2
    exit 1
  fi
done

echo "[engine] generic runtime and YAML command contracts"
(cd "$ENGINE_ROOT" && go test ./tests ./kernel ./contract)

echo "[cloud] control-plane and shared fixture contracts"
(cd "$CLOUD_ROOT" && go test ./...)

echo "[studio] workspace tests and production build"
(cd "$STUDIO_ROOT" && npm run test -- --run && npm run build)

echo "[website] shared fixture validation and production build"
(cd "$WEBSITE_ROOT" && npm run validate:contracts && npm run build)

echo "organizational runtime acceptance gates: passed"
