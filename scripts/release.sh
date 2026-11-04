#!/usr/bin/env bash
# Kigae Release Script
# Usage: ./scripts/release.sh v0.2.0
#
# Does:
# 1. Validates pre-tag checks (tests + lint)
# 2. Generates CHANGELOG.md entry
# 3. Bumps VERSION file
# 4. Creates and pushes tag
# 5. GitHub Actions builds binaries, creates release, and publishes Docker image

set -euo pipefail

TAG="${1:-}"
if [ -z "$TAG" ]; then
  echo "Usage: ./scripts/release.sh v0.2.0"
  exit 1
fi

# Strip 'v' prefix for version number
VERSION="${TAG#v}"

echo "=== Kigae Release: $TAG ==="

# --- Step 1: Pre-tag validation ---
echo ""
echo "[1/4] Running pre-tag checks..."

echo "  → Running tests..."
go test ./... 2>&1 | tail -1

echo "  → Running lint..."
golangci-lint run --timeout=2m 2>&1 | tail -3 || echo "  ⚠️  Lint warnings (non-blocking)"

echo "  → Building..."
go build -o /tmp/kora-check .

echo "  ✓ Pre-tag checks passed"

# --- Step 2: Changelog ---
echo ""
echo "[2/4] Generating changelog..."

PREV_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "")
CHANGELOG_ENTRY=""

if [ -z "$PREV_TAG" ]; then
  CHANGELOG_ENTRY="## $TAG — Initial Release"
else
  COMMITS=$(git log --oneline --no-merges "$PREV_TAG..HEAD")

  FEATURES=$(echo "$COMMITS" | grep -iE 'feat|add|implement|support|create|build' || echo "")
  FIXES=$(echo "$COMMITS" | grep -iE 'fix|resolve|patch|correct|repair' || echo "")
  SECURITY=$(echo "$COMMITS" | grep -iE 'security|auth|csrf|cors|vuln|protect|harden' || echo "")
  REFACTOR=$(echo "$COMMITS" | grep -iE 'refactor|clean|improve|optimize|simplify' || echo "")
  DOCS=$(echo "$COMMITS" | grep -iE 'docs|document|readme|guide' || echo "")

  CHANGELOG_ENTRY="## $TAG — $(date +%Y-%m-%d)"

  if [ -n "$FEATURES" ]; then
    CHANGELOG_ENTRY+=$'\n\n'"### Features"$'\n'
    while IFS= read -r line; do
      CHANGELOG_ENTRY+="- ${line#* }"$'\n'
    done <<< "$FEATURES"
  fi
  if [ -n "$FIXES" ]; then
    CHANGELOG_ENTRY+=$'\n'"### Fixes"$'\n'
    while IFS= read -r line; do
      CHANGELOG_ENTRY+="- ${line#* }"$'\n'
    done <<< "$FIXES"
  fi
  if [ -n "$SECURITY" ]; then
    CHANGELOG_ENTRY+=$'\n'"### Security"$'\n'
    while IFS= read -r line; do
      CHANGELOG_ENTRY+="- ${line#* }"$'\n'
    done <<< "$SECURITY"
  fi
  if [ -n "$REFACTOR" ]; then
    CHANGELOG_ENTRY+=$'\n'"### Improvements"$'\n'
    while IFS= read -r line; do
      CHANGELOG_ENTRY+="- ${line#* }"$'\n'
    done <<< "$REFACTOR"
  fi
  if [ -n "$DOCS" ]; then
    CHANGELOG_ENTRY+=$'\n'"### Documentation"$'\n'
    while IFS= read -r line; do
      CHANGELOG_ENTRY+="- ${line#* }"$'\n'
    done <<< "$DOCS"
  fi
fi

# Prepend to CHANGELOG.md
if [ -f CHANGELOG.md ]; then
  printf "%s\n\n%s\n" "$CHANGELOG_ENTRY" "$(cat CHANGELOG.md)" > CHANGELOG.md
else
  printf "%s\n" "$CHANGELOG_ENTRY" > CHANGELOG.md
fi

echo "  ✓ CHANGELOG.md updated"

# --- Step 3: Version bump ---
echo ""
echo "[3/4] Bumping version to $VERSION..."
echo "$VERSION" > VERSION
echo "  ✓ VERSION file updated"

# --- Step 4: Commit changelog + version, tag, push ---
echo ""
echo "[4/4] Committing and tagging..."

git add CHANGELOG.md VERSION
git commit -m "chore: release $TAG" 2>/dev/null || echo "  (nothing to commit)"

git tag -a "$TAG" -m "$TAG"
git push origin HEAD
git push origin "$TAG"

echo ""
echo "=== Release $TAG complete ==="
echo "GitHub Actions will build binaries, create the release, and publish Docker tags:"
echo "  smitdockerhub/kora:$VERSION"
echo "  smitdockerhub/kora:$TAG"
echo "  smitdockerhub/kora:latest"
