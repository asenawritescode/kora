#!/bin/bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo "=== Go Golden Tests ==="
cd "$ROOT" && go test ./doctype/ -run TestGoldenComputedVectors -v -count=1

echo ""
echo "=== JS Golden Parity Tests ==="
cd "$ROOT/ui" && npx vitest run --reporter=verbose

echo ""
echo "=== ALL GOLDEN TESTS PASSED ==="
