#!/usr/bin/env bash
# Compare two runs produced by run.sh.
#
# Usage: bench/redis-benchmark/compare.sh <label-a> <label-b>
#
# Labels are the suffixed variant produced by run.sh: <label>-<target>.
# Examples:
#   compare.sh phase-0-gocache phase-0-valkey          # gocache vs valkey at the same moment
#   compare.sh phase-0-gocache phase-3-gocache         # gocache progress across phases
#   compare.sh phase-0-valkey  phase-3-valkey          # valkey sanity — should be flat
set -euo pipefail

if [[ $# -ne 2 ]]; then
    echo "usage: $0 <label-a> <label-b>" >&2
    exit 64
fi

LABEL_A="$1"
LABEL_B="$2"
# Default results dir matches run.sh: bench/results/<branch>/. Override
# with RESULTS_DIR=... when comparing across branches.
REPO_ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
BRANCH="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
BRANCH_SAFE="${BRANCH//\//-}"
DIR="${RESULTS_DIR:-$REPO_ROOT/bench/results/$BRANCH_SAFE}"

for L in "$LABEL_A" "$LABEL_B"; do
    for F in "$DIR/$L.csv" "$DIR/$L-pipelined.csv" "$DIR/$L-memory.txt"; do
        [[ -r "$F" ]] || { echo "missing: $F" >&2; exit 1; }
    done
done

compare_csv() {
    local title="$1" file_a="$2" file_b="$3"
    echo
    echo "== $title =="
    python3 - "$file_a" "$file_b" "$LABEL_A" "$LABEL_B" <<'PY'
import csv, sys
a_path, b_path, a_label, b_label = sys.argv[1:5]

def load(p):
    out = {}
    with open(p) as f:
        r = csv.DictReader(f)
        for row in r:
            k = (row.get("test") or row.get("Test") or "").strip('"')
            def num(*fields, fallback=None):
                for f in fields:
                    v = row.get(f)
                    if v in (None, ""): continue
                    try: return float(v.strip('"'))
                    except ValueError: pass
                return fallback
            out[k] = {
                "rps": num("rps", "RequestsPerSecond"),
                "p50": num("p50_latency_ms", "p50"),
                "p99": num("p99_latency_ms", "p99"),
            }
    return out

a = load(a_path); b = load(b_path)
keys = sorted(set(a) | set(b))

hdr = f"{'command':<16} {a_label+' rps':>14} {b_label+' rps':>14} {'Δrps%':>9} {a_label+' p99':>12} {b_label+' p99':>12} {'Δp99%':>9}"
print(hdr)
print("-" * len(hdr))

def fmt(v): return "-" if v is None else f"{v:,.0f}"
def pct(x, y):
    if x in (None, 0) or y is None: return "-"
    return f"{(y-x)/x*100:+.1f}%"

for k in keys:
    ra, rb = a.get(k, {}).get("rps"), b.get(k, {}).get("rps")
    pa, pb = a.get(k, {}).get("p99"), b.get(k, {}).get("p99")
    print(f"{k:<16} {fmt(ra):>14} {fmt(rb):>14} {pct(ra,rb):>9} {fmt(pa):>12} {fmt(pb):>12} {pct(pa,pb):>9}")
PY
}

compare_csv "Standard" "$DIR/$LABEL_A.csv" "$DIR/$LABEL_B.csv"
compare_csv "Pipelined" "$DIR/$LABEL_A-pipelined.csv" "$DIR/$LABEL_B-pipelined.csv"

echo
echo "== Memory (RSS, container) =="
python3 - "$DIR/$LABEL_A-memory.txt" "$DIR/$LABEL_B-memory.txt" "$LABEL_A" "$LABEL_B" <<'PY'
import sys
def load(p):
    out = {}
    with open(p) as f:
        for line in f:
            if "=" in line:
                k, v = line.strip().split("=", 1)
                out[k.strip()] = v.strip()
    return out
a = load(sys.argv[1]); b = load(sys.argv[2])
la, lb = sys.argv[3], sys.argv[4]
keys = ["baseline_rss_bytes","post_standard_rss_bytes","final_rss_bytes","delta_rss_bytes"]
hdr = f"{'metric':<24} {la:>14} {lb:>14} {'Δ':>14}"
print(hdr); print("-" * len(hdr))
for k in keys:
    av, bv = a.get(k, "-"), b.get(k, "-")
    try: delta = f"{int(bv)-int(av):+,}"
    except (TypeError, ValueError): delta = "-"
    print(f"{k:<24} {av:>14} {bv:>14} {delta:>14}")
PY
