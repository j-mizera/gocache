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
