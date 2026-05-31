#!/usr/bin/env bash
# Containerized redis-benchmark harness for GoCache with IPC plugins enabled.
#
# Targets:
#   gocache-ipc       GoCache + prometheus IPC plugin
#   gocache-ipc-otel  GoCache + prometheus + instrumentation IPC plugins,
#                     with instrumentation exporting OTLP traces/logs to a
#                     local OpenTelemetry Collector nop pipeline
#
# Output shape matches run.sh so compare.sh can compare these captures with
# valkey and core gocache captures: <label>-<target>.csv,
# <label>-<target>-pipelined.csv, and <label>-<target>-memory.txt.
# Set BENCH_STATS=1 to add benchprobe JSON snapshots for startup,
# standard, and pipelined attribution windows.

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
    echo "usage: $0 <label> [--target gocache-ipc|gocache-ipc-otel]" >&2
    exit 64
fi
case "$TARGET" in
    gocache-ipc|gocache-ipc-otel) ;;
    *) echo "--target must be gocache-ipc or gocache-ipc-otel, got: $TARGET" >&2; exit 64 ;;
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
if [[ -n "${IPC_PLUGINS:-}" ]]; then
    IPC_PLUGINS="${IPC_PLUGINS}"
elif [[ "$TARGET" == "gocache-ipc-otel" ]]; then
    IPC_PLUGINS="prometheus instrumentation"
else
    IPC_PLUGINS="prometheus"
fi
OTEL_COLLECTOR_IMAGE="${OTEL_COLLECTOR_IMAGE:-otel/opentelemetry-collector-contrib:latest}"
OTEL_MEM_LIMIT="${BENCH_OTEL_MEM_LIMIT:-1g}"
BENCH_IPC_EVENT_MODE="${BENCH_IPC_EVENT_MODE:-full}"
case "$BENCH_IPC_EVENT_MODE" in
    full|events-off|bridge-off) ;;
    *) echo "BENCH_IPC_EVENT_MODE must be one of: full, events-off, bridge-off; got: $BENCH_IPC_EVENT_MODE" >&2; exit 64 ;;
esac
BENCH_STATS="${BENCH_STATS:-0}"
case "$BENCH_STATS" in
    1|true|TRUE|yes|YES|on|ON) BENCH_STATS_ENABLED=1 ;;
    *) BENCH_STATS_ENABLED=0 ;;
esac
if [[ "$BENCH_STATS_ENABLED" == "1" && " $IPC_PLUGINS " != *" benchprobe "* ]]; then
    IPC_PLUGINS="$IPC_PLUGINS benchprobe"
fi

REPO_ROOT="$(GIT_MASTER=1 git -C "$(dirname "$0")" rev-parse --show-toplevel)"
BRANCH="$(GIT_MASTER=1 git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
BRANCH_SAFE="${BRANCH//\//-}"
RESULTS_DIR="${RESULTS_DIR:-$REPO_ROOT/bench/results/$BRANCH_SAFE}"
NET="gocache-bench-net"
TARGET_NAME="gocache-bench-target-ipc"
OTEL_NAME="gocache-bench-otel"
CONFIG_FILE="$RESULTS_DIR/$LABEL-$TARGET-config.yaml"
OTEL_CONFIG_FILE="$RESULTS_DIR/$LABEL-$TARGET-otel-collector.yaml"
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
if [[ "$TARGET" == "gocache-ipc-otel" ]] && ! docker_cmd image inspect "$OTEL_COLLECTOR_IMAGE" >/dev/null 2>&1; then
    docker_cmd pull "$OTEL_COLLECTOR_IMAGE"
fi
docker_cmd network inspect "$NET" >/dev/null 2>&1 || docker_cmd network create --driver bridge "$NET" >/dev/null

docker_cmd rm -f "$TARGET_NAME" "$OTEL_NAME" >/dev/null 2>&1 || true

cleanup() {
    docker_cmd rm -f "$TARGET_NAME" "$OTEL_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

if [[ "$TARGET" == "gocache-ipc-otel" ]]; then
    cat > "$OTEL_CONFIG_FILE" <<EOF_OTEL
receivers:
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318

exporters:
  nop:

service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [nop]
    logs:
      receivers: [otlp]
      exporters: [nop]
EOF_OTEL

    echo "Starting OpenTelemetry Collector ($OTEL_COLLECTOR_IMAGE, mem=$OTEL_MEM_LIMIT)..."
    docker_cmd run -d \
        --name "$OTEL_NAME" \
        --network "$NET" \
        --memory "$OTEL_MEM_LIMIT" \
        --memory-swap "$OTEL_MEM_LIMIT" \
        -v "$OTEL_CONFIG_FILE:/etc/otelcol/config.yaml:ro" \
        "$OTEL_COLLECTOR_IMAGE" \
        --config=/etc/otelcol/config.yaml \
        >/dev/null
    sleep 1
    if ! docker_cmd ps --filter "name=^${OTEL_NAME}\$" --format '{{.Names}}' | grep -q "$OTEL_NAME"; then
        echo "error: OpenTelemetry Collector exited before benchmark. Logs:" >&2
        docker_cmd logs "$OTEL_NAME" >&2 || true
        exit 1
    fi
fi

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

if [[ "$TARGET" == "gocache-ipc-otel" ]]; then
    cat >> "$CONFIG_FILE" <<EOF_CFG
    instrumentation:
      failure_policy: "halt_server"
      priority: 20
      scopes:
        - "events"
      config:
        endpoint: "$OTEL_NAME:4318"
        service: "gocache-bench"
        timeout_ms: 3000
        insecure: true
        disabled: false
EOF_CFG
fi
if [[ "$BENCH_STATS_ENABLED" == "1" ]]; then
    cat >> "$CONFIG_FILE" <<EOF_CFG
    benchprobe:
      failure_policy: "halt_server"
      priority: 30
      scopes:
        - "server:query:health"
        - "server:query:bench.stats"
        - "server:query:plugin.ipc"
EOF_CFG
fi

# Keep plugin HTTP servers inside the target container for readiness checks and benchmark snapshots.
TARGET_ENV=(-e PROMETHEUS_PORT=":9100")
if [[ "$BENCH_STATS_ENABLED" == "1" ]]; then
    TARGET_ENV+=(-e GOCACHE_BENCH_STATS="true" -e BENCHPROBE_PORT=":9200")
fi
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

# Ensure the Prometheus IPC plugin is registered before measuring plugin overhead.
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
if [[ "$BENCH_STATS_ENABLED" == "1" ]]; then
    for _ in $(seq 1 100); do
        if docker_cmd exec "$TARGET_NAME" wget -q -O /dev/null http://127.0.0.1:9200/readyz >/dev/null 2>&1; then
            break
        fi
        sleep 0.1
    done
    if ! docker_cmd exec "$TARGET_NAME" wget -q -O /dev/null http://127.0.0.1:9200/readyz >/dev/null 2>&1; then
        echo "error: benchprobe did not become ready. Target logs:" >&2
        docker_cmd logs "$TARGET_NAME" >&2 || true
        exit 1
    fi
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

write_bench_snapshot() {
    if [[ "$BENCH_STATS_ENABLED" != "1" ]]; then
        return 0
    fi
    local file="$1"
    local reset="$2"
    docker_cmd exec "$TARGET_NAME" \
        wget -q -O - "http://127.0.0.1:9200/snapshot?reset=${reset}&include=all" \
        > "$file"
}

BASELINE_MEM_B=$(read_mem_bytes "$TARGET_NAME")
OTEL_BASELINE_MEM_B=""
if [[ "$TARGET" == "gocache-ipc-otel" ]]; then
    OTEL_BASELINE_MEM_B=$(read_mem_bytes "$OTEL_NAME")
fi
OUT_STD="$RESULTS_DIR/$LABEL-$TARGET.csv"
OUT_PIPE="$RESULTS_DIR/$LABEL-$TARGET-pipelined.csv"
MEM_FILE="$RESULTS_DIR/$LABEL-$TARGET-memory.txt"
BENCH_STATS_BASELINE_FILE=""
BENCH_STATS_STANDARD_FILE=""
BENCH_STATS_PIPELINED_FILE=""
if [[ "$BENCH_STATS_ENABLED" == "1" ]]; then
    BENCH_STATS_BASELINE_FILE="$RESULTS_DIR/$LABEL-$TARGET-benchstats-baseline.json"
    BENCH_STATS_STANDARD_FILE="$RESULTS_DIR/$LABEL-$TARGET-benchstats-standard.json"
    BENCH_STATS_PIPELINED_FILE="$RESULTS_DIR/$LABEL-$TARGET-benchstats-pipelined.json"
    write_bench_snapshot "$BENCH_STATS_BASELINE_FILE" true
fi

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
OTEL_POST_STD_MEM_B=""
if [[ "$TARGET" == "gocache-ipc-otel" ]]; then
    OTEL_POST_STD_MEM_B=$(read_mem_bytes "$OTEL_NAME")
fi
write_bench_snapshot "$BENCH_STATS_STANDARD_FILE" true

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
write_bench_snapshot "$BENCH_STATS_PIPELINED_FILE" false
OTEL_FINAL_MEM_B=""
OTEL_COLLECTOR_META=""
OTEL_CONFIG_META=""
OTEL_DELTA_MEM_B=""
if [[ "$TARGET" == "gocache-ipc-otel" ]]; then
    OTEL_FINAL_MEM_B=$(read_mem_bytes "$OTEL_NAME")
    OTEL_COLLECTOR_META="$OTEL_COLLECTOR_IMAGE"
    OTEL_CONFIG_META="$OTEL_CONFIG_FILE"
    OTEL_DELTA_MEM_B="$((OTEL_FINAL_MEM_B - OTEL_BASELINE_MEM_B))"
fi
GOCACHE_COMMIT="$(GIT_MASTER=1 git -C "$REPO_ROOT" rev-parse HEAD)"
GOCACHE_BRANCH="$(GIT_MASTER=1 git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD)"
cat > "$MEM_FILE" <<EOF_META
label=$LABEL
target=$TARGET
mode=ipc
gocache_ipc_image=$GOCACHE_IPC_IMAGE
ipc_plugins=$IPC_PLUGINS
otel_collector_image=$OTEL_COLLECTOR_META
ipc_event_mode=$BENCH_IPC_EVENT_MODE
gocache_commit=$GOCACHE_COMMIT
gocache_branch=$GOCACHE_BRANCH
config_file=$CONFIG_FILE
otel_config_file=$OTEL_CONFIG_META
n=$N
clients=$CLIENTS
keyspace=$KEYSPACE
pipeline=$PIPELINE
target_cpus=$TARGET_CPUS
client_cpus=$CLIENT_CPUS
mem_limit=$MEM_LIMIT
bench_stats_enabled=$BENCH_STATS_ENABLED
bench_stats_baseline_file=$BENCH_STATS_BASELINE_FILE
bench_stats_standard_file=$BENCH_STATS_STANDARD_FILE
bench_stats_pipelined_file=$BENCH_STATS_PIPELINED_FILE
baseline_rss_bytes=$BASELINE_MEM_B
post_standard_rss_bytes=$POST_STD_MEM_B
final_rss_bytes=$FINAL_MEM_B
delta_rss_bytes=$((FINAL_MEM_B - BASELINE_MEM_B))
otel_baseline_rss_bytes=$OTEL_BASELINE_MEM_B
otel_post_standard_rss_bytes=$OTEL_POST_STD_MEM_B
otel_final_rss_bytes=$OTEL_FINAL_MEM_B
otel_delta_rss_bytes=$OTEL_DELTA_MEM_B
EOF_META

echo
echo "Results:"
echo "  $OUT_STD"
echo "  $OUT_PIPE"
echo "  $MEM_FILE"
echo "  $CONFIG_FILE"
if [[ "$TARGET" == "gocache-ipc-otel" ]]; then
    echo "  $OTEL_CONFIG_FILE"
fi
if [[ "$BENCH_STATS_ENABLED" == "1" ]]; then
    echo "  $BENCH_STATS_BASELINE_FILE"
    echo "  $BENCH_STATS_STANDARD_FILE"
    echo "  $BENCH_STATS_PIPELINED_FILE"
fi
printf '  target memory: baseline=%d  post-standard=%d  final=%d  delta=%+d bytes\n' \
    "$BASELINE_MEM_B" "$POST_STD_MEM_B" "$FINAL_MEM_B" "$((FINAL_MEM_B - BASELINE_MEM_B))"
if [[ "$TARGET" == "gocache-ipc-otel" ]]; then
    printf '  otel memory:   baseline=%d  post-standard=%d  final=%d  delta=%+d bytes\n' \
        "$OTEL_BASELINE_MEM_B" "$OTEL_POST_STD_MEM_B" "$OTEL_FINAL_MEM_B" "$((OTEL_FINAL_MEM_B - OTEL_BASELINE_MEM_B))"
fi
