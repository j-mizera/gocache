#!/usr/bin/env bash
# Run the thesis comparison matrix:
#   1. valkey reference
#   2. gocache core with no IPC plugins
#   3. gocache with IPC prometheus metrics only
#
# Usage:
#   bench/redis-benchmark/run-matrix.sh <label>
#
# For a smoke baseline, lower BENCH_N first, e.g.:
#   BENCH_N=1000 BENCH_CLIENTS=5 bench/redis-benchmark/run-matrix.sh smoke

set -euo pipefail

LABEL="${1:-}"
if [[ -z "$LABEL" ]]; then
    echo "usage: $0 <label>" >&2
    exit 64
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

"$SCRIPT_DIR/run.sh" "$LABEL" --target valkey
"$SCRIPT_DIR/run.sh" "$LABEL" --target gocache
"$SCRIPT_DIR/run-ipc.sh" "$LABEL" --target gocache-ipc

cat <<EOF

Matrix complete. Useful comparisons:
  $SCRIPT_DIR/compare.sh $LABEL-gocache $LABEL-valkey
  $SCRIPT_DIR/compare.sh $LABEL-gocache $LABEL-gocache-ipc
EOF
