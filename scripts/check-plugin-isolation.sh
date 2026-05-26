#!/usr/bin/env bash
# Enforce import layering across the codebase:
#
#   api/       → stdlib only (+ protobuf for gcpc)
#   commons/   → api/, stdlib, external libs
#   sdk/       → api/, commons/
#   plugins/   → api/, sdk/, commons/
#   pkg/       → api/, commons/
#   cmd/       → anything
#   testkit/   → api/, commons/
#
# Exits non-zero with a list of offending lines if any violation is found.
# Run locally with `bash scripts/check-plugin-isolation.sh`; CI runs the same.

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

errors=0

check() {
  local dir="$1" pattern="$2" msg="$3"
  if [[ ! -d "$dir" ]]; then
    return
  fi
  local matches
  matches="$(grep -rn --include='*.go' "$pattern" "$dir/" || true)"
  if [[ -n "$matches" ]]; then
    echo "ERROR: $msg" >&2
    echo "$matches" >&2
    errors=$((errors + 1))
  fi
}

# plugins/ cannot import pkg/
check plugins '"gocache/pkg/' \
  "plugins/ may not import gocache/pkg/* — use gocache/api/*, gocache/sdk/*, or gocache/commons/*"

# sdk/ cannot import pkg/ or plugins/
check sdk '"gocache/pkg/' \
  "sdk/ may not import gocache/pkg/*"
check sdk '"gocache/plugins/' \
  "sdk/ may not import gocache/plugins/*"

# commons/ cannot import pkg/, sdk/, or plugins/
check commons '"gocache/pkg/' \
  "commons/ may not import gocache/pkg/*"
check commons '"gocache/sdk/' \
  "commons/ may not import gocache/sdk/*"
check commons '"gocache/plugins/' \
  "commons/ may not import gocache/plugins/*"

# api/ cannot import commons/, pkg/, sdk/, or plugins/
check api '"gocache/commons/' \
  "api/ may not import gocache/commons/*"
check api '"gocache/pkg/' \
  "api/ may not import gocache/pkg/*"
check api '"gocache/sdk/' \
  "api/ may not import gocache/sdk/*"
check api '"gocache/plugins/' \
  "api/ may not import gocache/plugins/*"

if [[ $errors -gt 0 ]]; then
  exit 1
fi

echo "import layering: OK"
