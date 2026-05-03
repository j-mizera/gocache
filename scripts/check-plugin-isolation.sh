#!/usr/bin/env bash
# Enforce plugin isolation: nothing under plugins/ may import gocache/pkg/.
# Plugins are external consumers; the contract surface lives in gocache/api/.
#
# Exits non-zero with a list of offending lines if any violation is found.
# Run locally with `bash scripts/check-plugin-isolation.sh`; CI runs the same.

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

if [[ ! -d plugins ]]; then
  echo "no plugins/ directory — nothing to check"
  exit 0
fi

# -r recursive, -n line numbers, --include limits to .go files. The pattern
# matches any import path rooted at gocache/pkg/ inside plugins/.
matches="$(grep -rn --include='*.go' '"gocache/pkg/' plugins/ || true)"

if [[ -n "$matches" ]]; then
  echo "ERROR: plugins/ may not import gocache/pkg/* — use gocache/api/* instead." >&2
  echo "$matches" >&2
  exit 1
fi

echo "plugin isolation: OK (no gocache/pkg/* imports under plugins/)"
