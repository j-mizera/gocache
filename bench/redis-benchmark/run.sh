#!/usr/bin/env bash
# Run redis-benchmark (or valkey-benchmark) against gocache and save CSV
# results under bench/redis-benchmark/results/<label>*.
#
# Usage: bench/redis-benchmark/run.sh <label>
# Label convention: phase-0, phase-1, phase-2, phase-3, or arbitrary tag.
set -euo pipefail

LABEL="${1:-}"
if [[ -z "$LABEL" ]]; then
    echo "usage: $0 <label>" >&2
    exit 64
fi

# Resolve the benchmark client (redis-benchmark or valkey-benchmark).
BENCH_CLIENT="${BENCH_CLIENT:-}"
if [[ -z "$BENCH_CLIENT" ]]; then
    for cand in redis-benchmark valkey-benchmark; do
        if command -v "$cand" >/dev/null 2>&1; then
            BENCH_CLIENT="$cand"
            break
        fi
    done
fi
if [[ -z "$BENCH_CLIENT" ]]; then
    cat >&2 <<EOF
error: neither redis-benchmark nor valkey-benchmark found on PATH.
Install one of:
  Arch:   sudo pacman -S valkey
  Debian: sudo apt install redis-tools
  macOS:  brew install redis          # or valkey
Override via BENCH_CLIENT=/path/to/binary if installed in a non-standard place.
EOF
    exit 2
fi

echo "Using benchmark client: $BENCH_CLIENT ($($BENCH_CLIENT --version 2>&1 | head -1))"

# Tunable via env vars, defaults chosen for ~1 min total runtime.
N="${BENCH_N:-100000}"
CLIENTS="${BENCH_CLIENTS:-50}"
KEYSPACE="${BENCH_KEYSPACE:-100000}"
PIPELINE="${BENCH_PIPELINE:-10}"
SUITE="${BENCH_SUITE:-ping_inline,ping_mbulk,set,get,incr,lpush,rpush,lpop,rpop,sadd,hset,spop,lrange_100,mset}"

REPO_ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
BIN="$REPO_ROOT/bin/gocache-bench"
RESULTS_DIR="$REPO_ROOT/bench/redis-benchmark/results"
mkdir -p "$RESULTS_DIR"

echo "Building gocache-server..."
(cd "$REPO_ROOT" && go build -trimpath -ldflags="-s -w" -o "$BIN" ./cmd/server)

# Pick an unused port without binding our own listener (race-free enough for bench).
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')

# Temp data dir so the server doesn't touch the repo root.
DATA_DIR=$(mktemp -d -t gocache-bench.XXXXXX)
trap '[[ -n "${GOCACHE_PID:-}" ]] && kill "$GOCACHE_PID" 2>/dev/null || true; rm -rf "$DATA_DIR"' EXIT

echo "Starting gocache on :$PORT (data dir: $DATA_DIR)..."
"$BIN" \
    --address "127.0.0.1" \
    --port "$PORT" \
    --snapshot-file "$DATA_DIR/snap.dat" \
    --load-on-startup=false \
    --log-level warn \
    > "$DATA_DIR/gocache.log" 2>&1 &
GOCACHE_PID=$!

# Wait for listener (max 5s).
for _ in $(seq 1 50); do
    if "$BENCH_CLIENT" -h 127.0.0.1 -p "$PORT" -n 1 ping >/dev/null 2>&1; then
        break
    fi
    sleep 0.1
done

if ! kill -0 "$GOCACHE_PID" 2>/dev/null; then
    echo "error: gocache failed to start; log follows:" >&2
    cat "$DATA_DIR/gocache.log" >&2
    exit 1
fi

# Capture starting RSS for the memory report.
read_rss_kb() { awk '/^VmRSS:/ {print $2}' "/proc/$GOCACHE_PID/status"; }
BASELINE_RSS_KB=$(read_rss_kb)

OUT_STD="$RESULTS_DIR/$LABEL.csv"
OUT_PIPE="$RESULTS_DIR/$LABEL-pipelined.csv"
MEM_FILE="$RESULTS_DIR/$LABEL-memory.txt"

echo "Running standard suite (n=$N, c=$CLIENTS, r=$KEYSPACE)..."
"$BENCH_CLIENT" \
    -h 127.0.0.1 -p "$PORT" \
    -n "$N" -c "$CLIENTS" -r "$KEYSPACE" \
    -t "$SUITE" \
    --csv \
    > "$OUT_STD"

POST_STD_RSS_KB=$(read_rss_kb)

echo "Running pipelined suite (P=$PIPELINE)..."
"$BENCH_CLIENT" \
    -h 127.0.0.1 -p "$PORT" \
    -n "$N" -c "$CLIENTS" -r "$KEYSPACE" \
    -P "$PIPELINE" \
    -t "$SUITE" \
    --csv \
    > "$OUT_PIPE"

FINAL_RSS_KB=$(read_rss_kb)

cat > "$MEM_FILE" <<EOF
label=$LABEL
client=$BENCH_CLIENT
gocache_commit=$(git -C "$REPO_ROOT" rev-parse HEAD)
gocache_branch=$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD)
n=$N
clients=$CLIENTS
keyspace=$KEYSPACE
pipeline=$PIPELINE
baseline_rss_kb=$BASELINE_RSS_KB
post_standard_rss_kb=$POST_STD_RSS_KB
final_rss_kb=$FINAL_RSS_KB
delta_rss_kb=$((FINAL_RSS_KB - BASELINE_RSS_KB))
EOF

echo
echo "Results:"
echo "  $OUT_STD"
echo "  $OUT_PIPE"
echo "  $MEM_FILE"
echo
echo "Memory (KB): baseline=$BASELINE_RSS_KB  after-standard=$POST_STD_RSS_KB  final=$FINAL_RSS_KB  delta=$((FINAL_RSS_KB - BASELINE_RSS_KB))"
