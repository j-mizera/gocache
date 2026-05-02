#!/usr/bin/env bash
# Concurrent valkey-benchmark harness — runs SET-only and GET-only client
# loops in parallel against one target server. Captures the cross-path
# interaction cost that single-test runs miss.
#
# `valkey-benchmark -t set,get` runs each test sequentially, so it never
# exposes simultaneous read+write pressure on the engine. This script
# launches two client containers at once, each running a single test,
# pinned to disjoint cpu sets so they don't fight each other for CPU.
#
# Output: two CSVs (one per role) plus a memory metadata file. The
# diagnosis SUMMARY synthesises numbers from both.
#
# Usage:
#   bench/redis-benchmark/run-mixed.sh <label> [--target gocache|valkey]
#
# Pipelining: BENCH_PIPELINE=10 by default (matches run.sh). Set
# BENCH_PIPELINE=1 for a non-pipelined cross-path run.

set -euo pipefail

# ---- Args --------------------------------------------------------------
LABEL="${1:-}"
TARGET="gocache"
shift || true
while [[ $# -gt 0 ]]; do
    case "$1" in
        --target) TARGET="$2"; shift 2 ;;
        -h|--help) grep -E '^#( |$)' "$0" | head -25; exit 0 ;;
        *) echo "unknown arg: $1" >&2; exit 64 ;;
    esac
done

if [[ -z "$LABEL" ]]; then
    echo "usage: $0 <label> [--target gocache|valkey]" >&2
    exit 64
fi
case "$TARGET" in
    gocache|valkey) ;;
    *) echo "--target must be gocache or valkey, got: $TARGET" >&2; exit 64 ;;
esac

# ---- Config ------------------------------------------------------------
N="${BENCH_N:-100000}"
CLIENTS_EACH="${BENCH_CLIENTS_EACH:-25}"   # 25 + 25 = 50 total, matching run.sh
KEYSPACE="${BENCH_KEYSPACE:-100000}"
PIPELINE="${BENCH_PIPELINE:-10}"
TARGET_CPUS="${BENCH_TARGET_CPUS:-0-3}"
# Split client cpus between the SET and GET clients so they don't crowd
# each other off-core. With 4-7 reserved for clients in run.sh, we give
# 4-5 to the SET client and 6-7 to the GET client.
SET_CLIENT_CPUS="${BENCH_SET_CLIENT_CPUS:-4-5}"
GET_CLIENT_CPUS="${BENCH_GET_CLIENT_CPUS:-6-7}"
MEM_LIMIT="${BENCH_MEM_LIMIT:-2g}"

VALKEY_IMAGE="${VALKEY_IMAGE:-valkey/valkey:8}"
GOCACHE_IMAGE="${GOCACHE_IMAGE:-gocache-bench:local}"

# ---- Setup -------------------------------------------------------------
REPO_ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
BRANCH="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
BRANCH_SAFE="${BRANCH//\//-}"
RESULTS_DIR="${RESULTS_DIR:-$REPO_ROOT/bench/results/$BRANCH_SAFE}"
NET="gocache-bench-net"
TARGET_NAME="gocache-bench-target-mixed"
mkdir -p "$RESULTS_DIR"

docker_cmd() { command docker "$@"; }

if [[ "$TARGET" == "gocache" ]]; then
    if [[ "${REBUILD:-0}" == "1" ]] || ! docker_cmd image inspect "$GOCACHE_IMAGE" >/dev/null 2>&1; then
        echo "Building $GOCACHE_IMAGE..."
        (cd "$REPO_ROOT" && docker_cmd build --build-arg PLUGINS="" -t "$GOCACHE_IMAGE" .)
    fi
fi

if ! docker_cmd image inspect "$VALKEY_IMAGE" >/dev/null 2>&1; then
    docker_cmd pull "$VALKEY_IMAGE"
fi

docker_cmd network inspect "$NET" >/dev/null 2>&1 || docker_cmd network create --driver bridge "$NET" >/dev/null

docker_cmd rm -f "$TARGET_NAME" >/dev/null 2>&1 || true

cleanup() {
    docker_cmd rm -f "$TARGET_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ---- Start target ------------------------------------------------------
if [[ "$TARGET" == "gocache" ]]; then
    echo "Starting gocache target ($GOCACHE_IMAGE, cpuset=$TARGET_CPUS, mem=$MEM_LIMIT)..."
    docker_cmd run -d \
        --name "$TARGET_NAME" \
        --network "$NET" \
        --cpuset-cpus "$TARGET_CPUS" \
        --memory "$MEM_LIMIT" \
        --memory-swap "$MEM_LIMIT" \
        "$GOCACHE_IMAGE" \
        --config= \
        --address 0.0.0.0 --port 6379 \
        --load-on-startup=false \
        --log-level warn \
        >/dev/null
else
    echo "Starting valkey target ($VALKEY_IMAGE, cpuset=$TARGET_CPUS, mem=$MEM_LIMIT)..."
    docker_cmd run -d \
        --name "$TARGET_NAME" \
        --network "$NET" \
        --cpuset-cpus "$TARGET_CPUS" \
        --memory "$MEM_LIMIT" \
        --memory-swap "$MEM_LIMIT" \
        "$VALKEY_IMAGE" \
        valkey-server --save "" --appendonly no --port 6379 \
        >/dev/null
fi

for _ in $(seq 1 50); do
    if docker_cmd run --rm --network "$NET" "$VALKEY_IMAGE" \
        valkey-benchmark -h "$TARGET_NAME" -p 6379 -n 1 ping >/dev/null 2>&1; then
        break
    fi
    sleep 0.1
done

if ! docker_cmd ps --filter "name=^${TARGET_NAME}\$" --format '{{.Names}}' | grep -q "$TARGET_NAME"; then
    echo "error: target container exited before readiness. Logs:" >&2
    docker_cmd logs "$TARGET_NAME" >&2 || true
    exit 1
fi

# ---- Pre-load keyspace so GET has hits ---------------------------------
echo "Pre-loading $KEYSPACE keys..."
docker_cmd run --rm \
    --network "$NET" \
    --cpuset-cpus "$SET_CLIENT_CPUS" \
    "$VALKEY_IMAGE" \
    valkey-benchmark \
        -h "$TARGET_NAME" -p 6379 \
        -n "$KEYSPACE" -c 50 -r "$KEYSPACE" \
        -t set --csv \
    >/dev/null

# ---- Memory baseline ---------------------------------------------------
read_mem_bytes() {
    local raw
    raw=$(docker_cmd stats --no-stream --format '{{.MemUsage}}' "$TARGET_NAME" 2>/dev/null | awk '{print $1}')
    python3 -c '
import sys, re
v = sys.argv[1]
m = re.match(r"([0-9.]+)([KMGTP]?i?B)", v)
if not m: print(0); raise SystemExit
n = float(m.group(1))
u = m.group(2)
mult = {"B":1,"KiB":1024,"MiB":1024**2,"GiB":1024**3,"TiB":1024**4,
        "KB":1000,"MB":1000**2,"GB":1000**3,"TB":1000**4}.get(u, 1)
print(int(n*mult))
' "$raw"
}

BASELINE_MEM_B=$(read_mem_bytes)

# ---- Concurrent SET + GET clients --------------------------------------
OUT_SET="$RESULTS_DIR/$LABEL-$TARGET-mixed-set.csv"
OUT_GET="$RESULTS_DIR/$LABEL-$TARGET-mixed-get.csv"
MEM_FILE="$RESULTS_DIR/$LABEL-$TARGET-mixed-memory.txt"

echo "Launching concurrent SET ($CLIENTS_EACH clients on cpu $SET_CLIENT_CPUS)..."
echo "             + concurrent GET ($CLIENTS_EACH clients on cpu $GET_CLIENT_CPUS)..."
echo "             P=$PIPELINE n=$N keyspace=$KEYSPACE"

# Run both in parallel, capture exit codes via wait.
docker_cmd run --rm \
    --network "$NET" \
    --cpuset-cpus "$SET_CLIENT_CPUS" \
    --memory "$MEM_LIMIT" \
    --memory-swap "$MEM_LIMIT" \
    "$VALKEY_IMAGE" \
    valkey-benchmark \
        -h "$TARGET_NAME" -p 6379 \
        -n "$N" -c "$CLIENTS_EACH" -r "$KEYSPACE" \
        -P "$PIPELINE" \
        -t set --csv \
    > "$OUT_SET" &
SET_PID=$!

docker_cmd run --rm \
    --network "$NET" \
    --cpuset-cpus "$GET_CLIENT_CPUS" \
    --memory "$MEM_LIMIT" \
    --memory-swap "$MEM_LIMIT" \
    "$VALKEY_IMAGE" \
    valkey-benchmark \
        -h "$TARGET_NAME" -p 6379 \
        -n "$N" -c "$CLIENTS_EACH" -r "$KEYSPACE" \
        -P "$PIPELINE" \
        -t get --csv \
    > "$OUT_GET" &
GET_PID=$!

set +e
wait "$SET_PID"; SET_EXIT=$?
wait "$GET_PID"; GET_EXIT=$?
set -e

if [[ $SET_EXIT -ne 0 || $GET_EXIT -ne 0 ]]; then
    echo "warning: client exited non-zero (set=$SET_EXIT, get=$GET_EXIT)" >&2
fi

FINAL_MEM_B=$(read_mem_bytes)

# ---- Metadata ----------------------------------------------------------
cat > "$MEM_FILE" <<EOF
label=$LABEL
target=$TARGET
mode=mixed-concurrent
valkey_image=$VALKEY_IMAGE
gocache_image=$GOCACHE_IMAGE
gocache_commit=$(git -C "$REPO_ROOT" rev-parse HEAD)
gocache_branch=$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD)
n=$N
clients_each=$CLIENTS_EACH
keyspace=$KEYSPACE
pipeline=$PIPELINE
target_cpus=$TARGET_CPUS
set_client_cpus=$SET_CLIENT_CPUS
get_client_cpus=$GET_CLIENT_CPUS
mem_limit=$MEM_LIMIT
baseline_rss_bytes=$BASELINE_MEM_B
final_rss_bytes=$FINAL_MEM_B
delta_rss_bytes=$((FINAL_MEM_B - BASELINE_MEM_B))
set_exit=$SET_EXIT
get_exit=$GET_EXIT
EOF

echo
echo "Results:"
echo "  $OUT_SET"
echo "  $OUT_GET"
echo "  $MEM_FILE"
printf '  memory: baseline=%d  final=%d  delta=%+d bytes\n' \
    "$BASELINE_MEM_B" "$FINAL_MEM_B" "$((FINAL_MEM_B - BASELINE_MEM_B))"

# Print rps from each CSV side-by-side for at-a-glance comparison.
echo
echo "Throughput (rps under concurrent mixed load):"
awk -F',' 'NR==1{next} {gsub(/"/,""); printf "  SET: %s\n", $2}' "$OUT_SET" 2>/dev/null || true
awk -F',' 'NR==1{next} {gsub(/"/,""); printf "  GET: %s\n", $2}' "$OUT_GET" 2>/dev/null || true
