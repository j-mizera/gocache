#!/usr/bin/env bash
# Compare two Pub/Sub runs produced by run-pubsub.sh.
#
# Usage: bench/redis-benchmark/compare-pubsub.sh <label-a-target> <label-b-target>
# Example:
#   compare-pubsub.sh baseline-valkey baseline-gocache-pubsub

set -euo pipefail

if [[ $# -ne 2 ]]; then
    echo "usage: $0 <label-a-target> <label-b-target>" >&2
    exit 64
fi

LABEL_A="$1"
LABEL_B="$2"
REPO_ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
BRANCH="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
BRANCH_SAFE="${BRANCH//\//-}"
DIR="${RESULTS_DIR:-$REPO_ROOT/bench/results/$BRANCH_SAFE}"

for label in "$LABEL_A" "$LABEL_B"; do
    for file in "$DIR/$label-pubsub.csv" "$DIR/$label-pubsub-memory.txt"; do
        [[ -r "$file" ]] || { echo "missing: $file" >&2; exit 1; }
    done
done

python3 - "$DIR/$LABEL_A-pubsub.csv" "$DIR/$LABEL_B-pubsub.csv" "$LABEL_A" "$LABEL_B" <<'PY'
import csv
import sys

a_path, b_path, a_label, b_label = sys.argv[1:5]

def load(path):
    out = {}
    with open(path, newline="") as f:
        for row in csv.DictReader(f):
            out[row["test"]] = {
                "rps": float(row["rps"]),
                "p50": float(row["p50_latency_ms"]),
                "p99": float(row["p99_latency_ms"]),
                "delivered": int(row["delivered_messages"]),
            }
    return out

def pct(a, b):
    if a == 0:
        return "-"
    return f"{(b-a)/a*100:+.1f}%"

def fmt(v):
    return f"{v:,.0f}"

a = load(a_path)
b = load(b_path)
keys = sorted(set(a) | set(b))

hdr = f"{'scenario':<22} {a_label+' rps':>18} {b_label+' rps':>18} {'Δrps%':>9} {a_label+' p99':>14} {b_label+' p99':>14} {'Δp99%':>9} {'delivered':>12}"
print("== Pub/Sub ==")
print(hdr)
print("-" * len(hdr))
for key in keys:
    av = a.get(key)
    bv = b.get(key)
    if not av or not bv:
        print(f"{key:<22} missing")
        continue
    delivered = "ok" if av["delivered"] == bv["delivered"] else f"{av['delivered']} vs {bv['delivered']}"
    print(f"{key:<22} {fmt(av['rps']):>18} {fmt(bv['rps']):>18} {pct(av['rps'], bv['rps']):>9} {av['p99']:>14.3f} {bv['p99']:>14.3f} {pct(av['p99'], bv['p99']):>9} {delivered:>12}")
PY

echo
echo "== Memory (RSS, container) =="
python3 - "$DIR/$LABEL_A-pubsub-memory.txt" "$DIR/$LABEL_B-pubsub-memory.txt" "$LABEL_A" "$LABEL_B" <<'PY'
import sys

def load(path):
    out = {}
    with open(path) as f:
        for line in f:
            if "=" in line:
                k, v = line.strip().split("=", 1)
                out[k] = v
    return out

a = load(sys.argv[1])
b = load(sys.argv[2])
la = sys.argv[3]
lb = sys.argv[4]
keys = ["baseline_rss_bytes", "final_rss_bytes", "delta_rss_bytes"]
hdr = f"{'metric':<24} {la:>18} {lb:>18} {'Δ':>18}"
print(hdr)
print("-" * len(hdr))
for key in keys:
    av = a.get(key, "-")
    bv = b.get(key, "-")
    try:
        delta = f"{int(bv)-int(av):+,}"
    except ValueError:
        delta = "-"
    print(f"{key:<24} {av:>18} {bv:>18} {delta:>18}")
PY
