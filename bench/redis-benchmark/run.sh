#!/usr/bin/env bash
# Containerized redis-benchmark harness for gocache vs valkey/redis.
#
# Both the target server and the benchmark client run in containers on a
# shared bridge network, with matching resource limits, so numbers for
# gocache and the reference stay directly comparable.
#
# Usage:
#   bench/redis-benchmark/run.sh <label> [--target gocache|valkey]
#
# Label convention: phase-0, phase-3, etc. The --target flag is appended
# to the label in the output filename so the same label works for both.
#
# Prereq: the current shell must have docker-group access. If `docker`
# returns permission denied on this host, either re-login after group
# membership takes effect or prefix the whole script with `sg docker -c`.
set -euo pipefail

# ---- Args --------------------------------------------------------------
LABEL="${1:-}"
TARGET="gocache"
shift || true
while [[ $# -gt 0 ]]; do
    case "$1" in
        --target) TARGET="$2"; shift 2 ;;
        -h|--help) grep -E '^#( |$)' "$0" | head -20; exit 0 ;;
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

# ---- Config (env overridable) ------------------------------------------
N="${BENCH_N:-100000}"
CLIENTS="${BENCH_CLIENTS:-50}"
KEYSPACE="${BENCH_KEYSPACE:-100000}"
PIPELINE="${BENCH_PIPELINE:-10}"
SUITE="${BENCH_SUITE:-ping_inline,ping_mbulk,set,get,incr,lpush,rpush,lpop,rpop,sadd,hset,spop,lrange_100,mset}"
TARGET_CPUS="${BENCH_TARGET_CPUS:-0-3}"
CLIENT_CPUS="${BENCH_CLIENT_CPUS:-4-7}"
MEM_LIMIT="${BENCH_MEM_LIMIT:-2g}"

# Valkey ships redis-benchmark-compatible client binary `valkey-benchmark`.
# We use the same image for both the client and the valkey server case so
# any version-skew concerns collapse to one image tag.
VALKEY_IMAGE="${VALKEY_IMAGE:-valkey/valkey:8}"
GOCACHE_IMAGE="${GOCACHE_IMAGE:-gocache-bench:local}"

# ---- Setup -------------------------------------------------------------
REPO_ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
# Results land in bench/results/<branch>/ so each branch's outputs are
# segregated. Override with RESULTS_DIR=... if you need a custom path.
BRANCH="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
BRANCH_SAFE="${BRANCH//\//-}"
RESULTS_DIR="${RESULTS_DIR:-$REPO_ROOT/bench/results/$BRANCH_SAFE}"
NET="gocache-bench-net"
TARGET_NAME="gocache-bench-target"
mkdir -p "$RESULTS_DIR"

docker_cmd() { command docker "$@"; }

# Build gocache image if missing or if --rebuild was requested via env.
if [[ "$TARGET" == "gocache" ]]; then
    if [[ "${REBUILD:-0}" == "1" ]] || ! docker_cmd image inspect "$GOCACHE_IMAGE" >/dev/null 2>&1; then
        echo "Building $GOCACHE_IMAGE..."
        (cd "$REPO_ROOT" && docker_cmd build --build-arg PLUGINS="" -t "$GOCACHE_IMAGE" .)
    fi
fi

# Pull the valkey image once.
if ! docker_cmd image inspect "$VALKEY_IMAGE" >/dev/null 2>&1; then
    docker_cmd pull "$VALKEY_IMAGE"
fi

# Ensure the shared network exists.
docker_cmd network inspect "$NET" >/dev/null 2>&1 || docker_cmd network create --driver bridge "$NET" >/dev/null

# Tear down any stale target container from a previous aborted run.
docker_cmd rm -f "$TARGET_NAME" >/dev/null 2>&1 || true

# ---- Start target ------------------------------------------------------
cleanup() {
    docker_cmd rm -f "$TARGET_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

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

# Wait for readiness via a ping from a throwaway client container.
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

# ---- Baseline memory ---------------------------------------------------
read_mem_bytes() {
    # docker stats --no-stream outputs e.g. "12.3MiB / 2GiB"; normalise to bytes.
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

# ---- Run suites --------------------------------------------------------
OUT_STD="$RESULTS_DIR/$LABEL-$TARGET.csv"
OUT_PIPE="$RESULTS_DIR/$LABEL-$TARGET-pipelined.csv"
MEM_FILE="$RESULTS_DIR/$LABEL-$TARGET-memory.txt"

echo "Running standard suite (target=$TARGET, n=$N, c=$CLIENTS, r=$KEYSPACE)..."
docker_cmd run --rm \
    --network "$NET" \
    --cpuset-cpus "$CLIENT_CPUS" \
    --memory "$MEM_LIMIT" \
    --memory-swap "$MEM_LIMIT" \
    "$VALKEY_IMAGE" \
    valkey-benchmark \
        -h "$TARGET_NAME" -p 6379 \
        -n "$N" -c "$CLIENTS" -r "$KEYSPACE" \
        -t "$SUITE" \
        --csv \
    > "$OUT_STD"

POST_STD_MEM_B=$(read_mem_bytes)

echo "Running pipelined suite (P=$PIPELINE)..."
docker_cmd run --rm \
    --network "$NET" \
    --cpuset-cpus "$CLIENT_CPUS" \
    --memory "$MEM_LIMIT" \
    --memory-swap "$MEM_LIMIT" \
    "$VALKEY_IMAGE" \
    valkey-benchmark \
        -h "$TARGET_NAME" -p 6379 \
        -n "$N" -c "$CLIENTS" -r "$KEYSPACE" \
        -P "$PIPELINE" \
        -t "$SUITE" \
        --csv \
    > "$OUT_PIPE"

FINAL_MEM_B=$(read_mem_bytes)

# ---- Write metadata ----------------------------------------------------
cat > "$MEM_FILE" <<EOF
label=$LABEL
target=$TARGET
valkey_image=$VALKEY_IMAGE
gocache_image=$GOCACHE_IMAGE
gocache_commit=$(git -C "$REPO_ROOT" rev-parse HEAD)
gocache_branch=$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD)
n=$N
clients=$CLIENTS
keyspace=$KEYSPACE
pipeline=$PIPELINE
target_cpus=$TARGET_CPUS
client_cpus=$CLIENT_CPUS
mem_limit=$MEM_LIMIT
baseline_rss_bytes=$BASELINE_MEM_B
post_standard_rss_bytes=$POST_STD_MEM_B
final_rss_bytes=$FINAL_MEM_B
delta_rss_bytes=$((FINAL_MEM_B - BASELINE_MEM_B))
EOF

echo
echo "Results:"
echo "  $OUT_STD"
echo "  $OUT_PIPE"
echo "  $MEM_FILE"
printf '  memory: baseline=%d  post-standard=%d  final=%d  delta=%+d bytes\n' \
    "$BASELINE_MEM_B" "$POST_STD_MEM_B" "$FINAL_MEM_B" "$((FINAL_MEM_B - BASELINE_MEM_B))"
