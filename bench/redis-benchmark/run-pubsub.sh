#!/usr/bin/env bash
# Pub/Sub benchmark harness for Valkey vs GoCache's IPC pubsub plugin.
#
# This workload is intentionally separate from valkey-benchmark's generic
# command suite: Pub/Sub correctness and cost depend on subscriber fanout, so
# the client opens real SUBSCRIBE connections, waits for subscription acks,
# publishes messages, and verifies every subscriber receives every message.
#
# Usage:
#   bench/redis-benchmark/run-pubsub.sh <label> [--target valkey|gocache-pubsub]

set -euo pipefail

LABEL="${1:-}"
TARGET="gocache-pubsub"
shift || true
while [[ $# -gt 0 ]]; do
    case "$1" in
        --target) TARGET="$2"; shift 2 ;;
        -h|--help) grep -E '^#( |$)' "$0" | head -28; exit 0 ;;
        *) echo "unknown arg: $1" >&2; exit 64 ;;
    esac
done

if [[ -z "$LABEL" ]]; then
    echo "usage: $0 <label> [--target valkey|gocache-pubsub]" >&2
    exit 64
fi
case "$TARGET" in
    valkey|gocache-pubsub) ;;
    *) echo "--target must be valkey or gocache-pubsub, got: $TARGET" >&2; exit 64 ;;
esac

N="${BENCH_PUBSUB_N:-10000}"
FANOUTS="${BENCH_PUBSUB_FANOUTS:-0,1,10}"
CHANNEL="${BENCH_PUBSUB_CHANNEL:-bench:pubsub}"
MESSAGE_BYTES="${BENCH_PUBSUB_MESSAGE_BYTES:-32}"
TARGET_CPUS="${BENCH_TARGET_CPUS:-0-3}"
CLIENT_CPUS="${BENCH_CLIENT_CPUS:-4-7}"
MEM_LIMIT="${BENCH_MEM_LIMIT:-2g}"
GOCACHE_MAX_MEMORY_MB="${BENCH_GOCACHE_MAX_MEMORY_MB:-1024}"

VALKEY_IMAGE="${VALKEY_IMAGE:-valkey/valkey:8}"
GOCACHE_PUBSUB_IMAGE="${GOCACHE_PUBSUB_IMAGE:-gocache-bench:local-pubsub}"
PYTHON_IMAGE="${BENCH_PYTHON_IMAGE:-python:3.12-alpine}"

REPO_ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
BRANCH="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
BRANCH_SAFE="${BRANCH//\//-}"
RESULTS_DIR="${RESULTS_DIR:-$REPO_ROOT/bench/results/$BRANCH_SAFE}"
NET="gocache-bench-net"
TARGET_NAME="gocache-bench-target-pubsub"
CONFIG_FILE="$RESULTS_DIR/$LABEL-$TARGET-config.yaml"
OUT_CSV="$RESULTS_DIR/$LABEL-$TARGET-pubsub.csv"
MEM_FILE="$RESULTS_DIR/$LABEL-$TARGET-pubsub-memory.txt"
CLIENT_SCRIPT="$RESULTS_DIR/$LABEL-$TARGET-pubsub-client.py"
mkdir -p "$RESULTS_DIR"

docker_cmd() { command docker "$@"; }

if [[ "$TARGET" == "gocache-pubsub" ]]; then
    if [[ "${REBUILD:-0}" == "1" ]] || ! docker_cmd image inspect "$GOCACHE_PUBSUB_IMAGE" >/dev/null 2>&1; then
        echo "Building $GOCACHE_PUBSUB_IMAGE with IPC_PLUGINS=pubsub..."
        (cd "$REPO_ROOT" && docker_cmd build \
            -f bench/redis-benchmark/Dockerfile.ipc \
            --build-arg IPC_PLUGINS="pubsub" \
            -t "$GOCACHE_PUBSUB_IMAGE" .)
    fi
fi

for image in "$VALKEY_IMAGE" "$PYTHON_IMAGE"; do
    if ! docker_cmd image inspect "$image" >/dev/null 2>&1; then
        docker_cmd pull "$image"
    fi
done

docker_cmd network inspect "$NET" >/dev/null 2>&1 || docker_cmd network create --driver bridge "$NET" >/dev/null
docker_cmd rm -f "$TARGET_NAME" >/dev/null 2>&1 || true

cleanup() {
    docker_cmd rm -f "$TARGET_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

if [[ "$TARGET" == "gocache-pubsub" ]]; then
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
    pubsub:
      failure_policy: "halt_server"
      priority: 1
      scopes:
        - "write"
        - "hook:pre"
        - "events"
EOF_CFG

    echo "Starting gocache-pubsub ($GOCACHE_PUBSUB_IMAGE, cpuset=$TARGET_CPUS, mem=$MEM_LIMIT)..."
    docker_cmd run -d \
        --name "$TARGET_NAME" \
        --network "$NET" \
        --cpuset-cpus "$TARGET_CPUS" \
        --memory "$MEM_LIMIT" \
        --memory-swap "$MEM_LIMIT" \
        -v "$CONFIG_FILE:/bench/gocache-pubsub.yaml:ro" \
        "$GOCACHE_PUBSUB_IMAGE" \
        --config /bench/gocache-pubsub.yaml \
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

for _ in $(seq 1 100); do
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

# Plugin readiness: PUBLISH must be registered and return an integer, not an
# unknown-command error. Valkey also passes this check.
for _ in $(seq 1 100); do
    if docker_cmd run --rm --network "$NET" "$VALKEY_IMAGE" \
        valkey-cli -h "$TARGET_NAME" -p 6379 PUBLISH "$CHANNEL" warmup >/dev/null 2>&1; then
        break
    fi
    sleep 0.1
done
if ! docker_cmd run --rm --network "$NET" "$VALKEY_IMAGE" \
    valkey-cli -h "$TARGET_NAME" -p 6379 PUBLISH "$CHANNEL" warmup >/dev/null 2>&1; then
    echo "error: PUBLISH readiness check failed. Target logs:" >&2
    docker_cmd logs "$TARGET_NAME" >&2 || true
    exit 1
fi

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

cat > "$CLIENT_SCRIPT" <<'PY'
import csv
import os
import socket
import statistics
import sys
import threading
import time

HOST = os.environ.get("TARGET_HOST", "gocache-bench-target-pubsub")
PORT = int(os.environ.get("TARGET_PORT", "6379"))
N = int(os.environ["BENCH_N"])
FANOUTS = [int(x) for x in os.environ["BENCH_FANOUTS"].split(",") if x.strip()]
CHANNEL = os.environ["BENCH_CHANNEL"]
MESSAGE = "x" * int(os.environ["BENCH_MESSAGE_BYTES"])
OUT = os.environ["BENCH_OUT"]
TIMEOUT = float(os.environ.get("BENCH_TIMEOUT_SECONDS", "30"))


def encode(*parts):
    out = [f"*{len(parts)}\r\n".encode()]
    for part in parts:
        if isinstance(part, bytes):
            data = part
        else:
            data = str(part).encode()
        out.append(f"${len(data)}\r\n".encode())
        out.append(data)
        out.append(b"\r\n")
    return b"".join(out)


class Resp:
    def __init__(self, sock):
        self.sock = sock
        self.file = sock.makefile("rb", buffering=0)

    def read(self):
        prefix = self.file.read(1)
        if not prefix:
            raise EOFError("connection closed")
        line = self.file.readline()
        if not line.endswith(b"\r\n"):
            raise ValueError("malformed RESP line")
        text = line[:-2]
        if prefix == b"+":
            return text.decode()
        if prefix == b"-":
            raise RuntimeError(text.decode())
        if prefix == b":":
            return int(text)
        if prefix == b"$":
            size = int(text)
            if size == -1:
                return None
            data = self.file.read(size)
            crlf = self.file.read(2)
            if crlf != b"\r\n":
                raise ValueError("malformed bulk string")
            return data.decode()
        if prefix == b"*":
            count = int(text)
            if count == -1:
                return None
            return [self.read() for _ in range(count)]
        raise ValueError(f"unknown RESP prefix {prefix!r}")


def connect():
    sock = socket.create_connection((HOST, PORT), timeout=TIMEOUT)
    sock.settimeout(TIMEOUT)
    return sock, Resp(sock)


def subscriber(index, expected, ready, done, errors):
    try:
        sock, resp = connect()
        sock.sendall(encode("SUBSCRIBE", CHANNEL))
        ack = resp.read()
        if not isinstance(ack, list) or ack[0] != "subscribe" or ack[1] != CHANNEL:
            raise RuntimeError(f"bad subscribe ack: {ack!r}")
        ready.set()
        received = 0
        while received < expected:
            msg = resp.read()
            if not isinstance(msg, list) or msg[0] != "message" or msg[1] != CHANNEL:
                raise RuntimeError(f"bad message for subscriber {index}: {msg!r}")
            received += 1
        sock.sendall(encode("UNSUBSCRIBE", CHANNEL))
        try:
            resp.read()
        except Exception:
            pass
        sock.close()
    except Exception as exc:
        errors.append(f"subscriber {index}: {exc}")
    finally:
        done.set()


def percentile(sorted_values, pct):
    if not sorted_values:
        return 0.0
    idx = int(round((pct / 100.0) * (len(sorted_values) - 1)))
    return sorted_values[max(0, min(idx, len(sorted_values) - 1))]


rows = []
for fanout in FANOUTS:
    ready_events = []
    done_events = []
    errors = []
    threads = []
    for i in range(fanout):
        ready = threading.Event()
        done = threading.Event()
        ready_events.append(ready)
        done_events.append(done)
        t = threading.Thread(target=subscriber, args=(i, N, ready, done, errors), daemon=True)
        threads.append(t)
        t.start()

    for ready in ready_events:
        if not ready.wait(TIMEOUT):
            errors.append("subscriber readiness timeout")
            break
    if errors:
        raise SystemExit("; ".join(errors))

    pub_sock, pub_resp = connect()
    latencies_ms = []
    start = time.perf_counter()
    for _ in range(N):
        before = time.perf_counter_ns()
        pub_sock.sendall(encode("PUBLISH", CHANNEL, MESSAGE))
        delivered = pub_resp.read()
        after = time.perf_counter_ns()
        if delivered != fanout:
            raise RuntimeError(f"PUBLISH returned {delivered}, want {fanout}")
        latencies_ms.append((after - before) / 1_000_000.0)
    elapsed = time.perf_counter() - start
    pub_sock.close()

    for done in done_events:
        if not done.wait(TIMEOUT):
            errors.append("subscriber delivery timeout")
    if errors:
        raise SystemExit("; ".join(errors))

    ordered = sorted(latencies_ms)
    rows.append({
        "test": f"PUBLISH_fanout_{fanout}",
        "rps": N / elapsed if elapsed > 0 else 0.0,
        "p50_latency_ms": percentile(ordered, 50),
        "p99_latency_ms": percentile(ordered, 99),
        "requests": N,
        "subscribers": fanout,
        "delivered_messages": N * fanout,
    })

with open(OUT, "w", newline="") as f:
    writer = csv.DictWriter(f, fieldnames=["test", "rps", "p50_latency_ms", "p99_latency_ms", "requests", "subscribers", "delivered_messages"])
    writer.writeheader()
    writer.writerows(rows)

for row in rows:
    print(f"{row['test']}: {row['rps']:.0f} rps p50={row['p50_latency_ms']:.3f}ms p99={row['p99_latency_ms']:.3f}ms")
PY

echo "Running Pub/Sub benchmark (target=$TARGET, n=$N, fanouts=$FANOUTS)..."
docker_cmd run --rm \
    --network "$NET" \
    --cpuset-cpus "$CLIENT_CPUS" \
    --memory "$MEM_LIMIT" \
    --memory-swap "$MEM_LIMIT" \
    -v "$CLIENT_SCRIPT:/bench/pubsub-client.py:ro" \
    -v "$RESULTS_DIR:/bench/results" \
    -e TARGET_HOST="$TARGET_NAME" \
    -e TARGET_PORT=6379 \
    -e BENCH_N="$N" \
    -e BENCH_FANOUTS="$FANOUTS" \
    -e BENCH_CHANNEL="$CHANNEL" \
    -e BENCH_MESSAGE_BYTES="$MESSAGE_BYTES" \
    -e BENCH_OUT="/bench/results/$(basename "$OUT_CSV")" \
    "$PYTHON_IMAGE" \
    python /bench/pubsub-client.py

FINAL_MEM_B=$(read_mem_bytes)

cat > "$MEM_FILE" <<EOF_META
label=$LABEL
target=$TARGET
mode=pubsub
valkey_image=$VALKEY_IMAGE
gocache_pubsub_image=$GOCACHE_PUBSUB_IMAGE
python_image=$PYTHON_IMAGE
gocache_commit=$(git -C "$REPO_ROOT" rev-parse HEAD)
gocache_branch=$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD)
config_file=$([[ "$TARGET" == "gocache-pubsub" ]] && printf '%s' "$CONFIG_FILE" || true)
n=$N
fanouts=$FANOUTS
channel=$CHANNEL
message_bytes=$MESSAGE_BYTES
target_cpus=$TARGET_CPUS
client_cpus=$CLIENT_CPUS
mem_limit=$MEM_LIMIT
baseline_rss_bytes=$BASELINE_MEM_B
final_rss_bytes=$FINAL_MEM_B
delta_rss_bytes=$((FINAL_MEM_B - BASELINE_MEM_B))
EOF_META

echo
echo "Results:"
echo "  $OUT_CSV"
echo "  $MEM_FILE"
if [[ "$TARGET" == "gocache-pubsub" ]]; then
    echo "  $CONFIG_FILE"
fi
printf '  memory: baseline=%d  final=%d  delta=%+d bytes\n' \
    "$BASELINE_MEM_B" "$FINAL_MEM_B" "$((FINAL_MEM_B - BASELINE_MEM_B))"
