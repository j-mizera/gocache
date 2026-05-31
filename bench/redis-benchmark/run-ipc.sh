#!/usr/bin/env bash
# Containerized redis-benchmark harness for GoCache with IPC plugins enabled.
#
# Targets:
#   gocache-ipc       GoCache + prometheus IPC plugin
#
# Output shape matches run.sh so compare.sh can compare these captures with
# valkey and core gocache captures: <label>-<target>.csv,
# <label>-<target>-pipelined.csv, and <label>-<target>-memory.txt.

set -euo pipefail

LABEL="${1:-}"
TARGET="gocache-ipc"
shift || true
while [[ $# -gt 0 ]]; do
    case "$1" in
        --target) TARGET="$2"; shift 2 ;;
        -h|--help) grep -E '^#( |$)' "$0" | head -30; exit 0 ;;
        *) echo "unknown arg: $1" >&2; exit 64 ;;
    esac
done

if [[ -z "$LABEL" ]]; then
    echo "usage: $0 <label> [--target gocache-ipc]" >&2
    exit 64
fi
case "$TARGET" in
    gocache-ipc) ;;
    *) echo "--target must be gocache-ipc, got: $TARGET" >&2; exit 64 ;;
esac

N="${BENCH_N:-100000}"
CLIENTS="${BENCH_CLIENTS:-50}"
KEYSPACE="${BENCH_KEYSPACE:-100000}"
PIPELINE="${BENCH_PIPELINE:-10}"
SUITE="${BENCH_SUITE:-ping_inline,ping_mbulk,set,get,incr,lpush,rpush,lpop,rpop,sadd,hset,spop,lrange_100,mset}"
TARGET_CPUS="${BENCH_TARGET_CPUS:-0-3}"
CLIENT_CPUS="${BENCH_CLIENT_CPUS:-4-7}"
MEM_LIMIT="${BENCH_MEM_LIMIT:-2g}"
GOCACHE_MAX_MEMORY_MB="${BENCH_GOCACHE_MAX_MEMORY_MB:-1024}"

VALKEY_IMAGE="${VALKEY_IMAGE:-valkey/valkey:8}"
GOCACHE_IPC_IMAGE="${GOCACHE_IPC_IMAGE:-gocache-bench:local-ipc}"
IPC_PLUGINS="${IPC_PLUGINS:-prometheus}"
BENCH_IPC_EVENT_MODE="${BENCH_IPC_EVENT_MODE:-full}"
case "$BENCH_IPC_EVENT_MODE" in
    full|events-off|bridge-off) ;;
    *) echo "BENCH_IPC_EVENT_MODE must be one of: full, events-off, bridge-off; got: $BENCH_IPC_EVENT_MODE" >&2; exit 64 ;;
esac

REPO_ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
BRANCH="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
BRANCH_SAFE="${BRANCH//\//-}"
RESULTS_DIR="${RESULTS_DIR:-$REPO_ROOT/bench/results/$BRANCH_SAFE}"
NET="gocache-bench-net"
TARGET_NAME="gocache-bench-target-ipc"
CONFIG_FILE="$RESULTS_DIR/$LABEL-$TARGET-config.yaml"
mkdir -p "$RESULTS_DIR"

PROMETHEUS_EVENT_SCOPE='        - "events"'
if [[ "$BENCH_IPC_EVENT_MODE" == "events-off" ]]; then
    PROMETHEUS_EVENT_SCOPE='        # events scope disabled by BENCH_IPC_EVENT_MODE=events-off'
fi

docker_cmd() { command docker "$@"; }

if [[ "${REBUILD:-0}" == "1" ]] || ! docker_cmd image inspect "$GOCACHE_IPC_IMAGE" >/dev/null 2>&1; then
    echo "Building $GOCACHE_IPC_IMAGE with IPC_PLUGINS=$IPC_PLUGINS..."
    (cd "$REPO_ROOT" && docker_cmd build \
        -f bench/redis-benchmark/Dockerfile.ipc \
        --build-arg IPC_PLUGINS="$IPC_PLUGINS" \
        -t "$GOCACHE_IPC_IMAGE" .)
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

cat > "$CONFIG_FILE" <<EOF_CFG
server:
  address: "0.0.0.0"
  port: 6379
  log_level: "warn"

persistence:
  load_on_startup: false

memory:
  max_memory_mb: $GOCACHE_MAX_MEMORY_MB
  eviction_policy: "lru"

plugins:
  enabled: true
  dir: "/usr/local/lib/gocache/plugins"
  socket_path: "/tmp/gocache-plugins.sock"
  health_interval: 10s
  shutdown_timeout: 5s
  max_restarts: 0
  connect_timeout: 10s
  min_restart_interval_for_replay: 30s
  overrides:
    prometheus:
      failure_policy: "halt_server"
      priority: 10
      scopes:
$PROMETHEUS_EVENT_SCOPE
        - "server:query:health"
        - "server:query:plugins"
        - "server:query:metrics.commands"
EOF_CFG

# Keep the metrics server inside the target container for readiness checks.
TARGET_ENV=(-e PROMETHEUS_PORT=":9100")
if [[ "$BENCH_IPC_EVENT_MODE" == "bridge-off" ]]; then
    TARGET_ENV+=(-e GOCACHE_BENCH_EVENT_BRIDGE_MODE="bridge-off")
fi

echo "Starting $TARGET ($GOCACHE_IPC_IMAGE, cpuset=$TARGET_CPUS, mem=$MEM_LIMIT)..."
docker_cmd run -d \
    --name "$TARGET_NAME" \
    --network "$NET" \
    --cpuset-cpus "$TARGET_CPUS" \
    --memory "$MEM_LIMIT" \
    --memory-swap "$MEM_LIMIT" \
    -v "$CONFIG_FILE:/bench/gocache-ipc.yaml:ro" \
    "${TARGET_ENV[@]}" \
    "$GOCACHE_IPC_IMAGE" \
    --config /bench/gocache-ipc.yaml \
    --address 0.0.0.0 --port 6379 \
    --load-on-startup=false \
    --log-level warn \
    >/dev/null

for _ in $(seq 1 50); do
    if docker_cmd run --rm --network "$NET" "$VALKEY_IMAGE" \
        valkey-benchmark -h "$TARGET_NAME" -p 6379 -n 1 ping >/dev/null 2>&1; then
        break
    fi
    sleep 0.1
done

if ! docker_cmd ps --filter "name=^${TARGET_NAME}\$" --format '{{.Names}}' | grep -q "$TARGET_NAME"; then
    echo "error: target container exited before RESP readiness. Logs:" >&2
    docker_cmd logs "$TARGET_NAME" >&2 || true
    exit 1
fi

# Ensure the IPC plugin is registered before measuring hook overhead.
for _ in $(seq 1 100); do
    if docker_cmd exec "$TARGET_NAME" wget -q -O /dev/null http://127.0.0.1:9100/readyz >/dev/null 2>&1; then
        break
    fi
    sleep 0.1
done
if ! docker_cmd exec "$TARGET_NAME" wget -q -O /dev/null http://127.0.0.1:9100/readyz >/dev/null 2>&1; then
    echo "error: prometheus did not become ready. Target logs:" >&2
    docker_cmd logs "$TARGET_NAME" >&2 || true
    exit 1
fi

read_mem_bytes() {
    local name="$1"
    local raw
    raw=$(docker_cmd stats --no-stream --format '{{.MemUsage}}' "$name" 2>/dev/null | awk '{print $1}')
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

BASELINE_MEM_B=$(read_mem_bytes "$TARGET_NAME")
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

POST_STD_MEM_B=$(read_mem_bytes "$TARGET_NAME")
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

FINAL_MEM_B=$(read_mem_bytes "$TARGET_NAME")
cat > "$MEM_FILE" <<EOF_META
label=$LABEL
target=$TARGET
mode=ipc
gocache_ipc_image=$GOCACHE_IPC_IMAGE
ipc_plugins=$IPC_PLUGINS
ipc_event_mode=$BENCH_IPC_EVENT_MODE
gocache_commit=$(git -C "$REPO_ROOT" rev-parse HEAD)
gocache_branch=$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD)
config_file=$CONFIG_FILE
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
EOF_META

echo
echo "Results:"
echo "  $OUT_STD"
echo "  $OUT_PIPE"
echo "  $MEM_FILE"
echo "  $CONFIG_FILE"
printf '  target memory: baseline=%d  post-standard=%d  final=%d  delta=%+d bytes\n' \
    "$BASELINE_MEM_B" "$POST_STD_MEM_B" "$FINAL_MEM_B" "$((FINAL_MEM_B - BASELINE_MEM_B))"
