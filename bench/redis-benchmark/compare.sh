#!/usr/bin/env bash
# Compare two runs produced by run.sh. Prints a side-by-side table of rps +
# p99 latency deltas for each command in both the standard and pipelined
# suites, plus the RSS delta.
#
# Usage: bench/redis-benchmark/compare.sh <label-a> <label-b>
set -euo pipefail

if [[ $# -ne 2 ]]; then
    echo "usage: $0 <label-a> <label-b>" >&2
    exit 64
fi

LABEL_A="$1"
LABEL_B="$2"
DIR="$(dirname "$0")/results"

for L in "$LABEL_A" "$LABEL_B"; do
    for F in "$DIR/$L.csv" "$DIR/$L-pipelined.csv" "$DIR/$L-memory.txt"; do
        [[ -r "$F" ]] || { echo "missing: $F" >&2; exit 1; }
    done
done

compare_csv() {
    local label="$1" file_a="$2" file_b="$3"
    echo
    echo "== $label =="
    printf '%-16s %14s %14s %10s %14s %14s %10s\n' \
        "command" \
        "rps-$LABEL_A" "rps-$LABEL_B" "rps-∆%" \
        "p99-$LABEL_A" "p99-$LABEL_B" "p99-∆%"
    # redis-benchmark --csv columns:
    # "test","rps","avg_lat","min_lat","p50","p95","p99","max_lat"
    # Some builds also emit percentile columns in different orders; we read
    # by header to stay compatible with valkey-benchmark future releases.
    python3 - "$file_a" "$file_b" <<'PY'
import csv, sys
a_path, b_path = sys.argv[1], sys.argv[2]
def load(p):
    out = {}
    with open(p) as f:
        r = csv.DictReader(f)
        for row in r:
            k = (row.get("test") or row.get("Test") or "").strip('"')
            def n(field_candidates, fallback=None):
                for f in field_candidates:
                    if f in row and row[f] not in ("", None):
                        try:
                            return float(row[f].strip('"'))
                        except ValueError:
                            pass
                return fallback
            out[k] = {
                "rps": n(["rps","RequestsPerSecond","requests per second"]),
                "p99": n(["p99_latency_ms","p99","P99","p99 latency (ms)"]),
            }
    return out
a = load(a_path); b = load(b_path)
keys = sorted(set(a) | set(b))
for k in keys:
    ra, rb = a.get(k, {}).get("rps"), b.get(k, {}).get("rps")
    pa, pb = a.get(k, {}).get("p99"), b.get(k, {}).get("p99")
    def fmt(v): return "-" if v is None else f"{v:,.0f}"
    def pct(x, y):
        if x is None or y is None or x == 0: return "-"
        return f"{(y-x)/x*100:+.1f}%"
    print(f"{k:<16} {fmt(ra):>14} {fmt(rb):>14} {pct(ra,rb):>10} {fmt(pa):>14} {fmt(pb):>14} {pct(pa,pb):>10}")
PY
}

compare_csv "Standard" "$DIR/$LABEL_A.csv" "$DIR/$LABEL_B.csv"
compare_csv "Pipelined" "$DIR/$LABEL_A-pipelined.csv" "$DIR/$LABEL_B-pipelined.csv"

echo
echo "== Memory =="
awk -F= '
    BEGIN { print "metric                 " }
    /^(baseline_rss_kb|post_standard_rss_kb|final_rss_kb|delta_rss_kb)=/ {
        metric = $1
        val = $2
        runs[metric]["a"] = (runs[metric]["a"] == "") ? val : runs[metric]["a"]
        runs[metric]["b"] = (runs[metric]["b"] == "") ? "" : val
    }
' /dev/null # placeholder — we'll do this in python instead.

python3 - "$DIR/$LABEL_A-memory.txt" "$DIR/$LABEL_B-memory.txt" <<'PY'
import sys
def load(p):
    out = {}
    with open(p) as f:
        for line in f:
            line = line.strip()
            if "=" in line:
                k, v = line.split("=", 1)
                out[k.strip()] = v.strip()
    return out
a = load(sys.argv[1]); b = load(sys.argv[2])
keys = ["baseline_rss_kb","post_standard_rss_kb","final_rss_kb","delta_rss_kb"]
print(f"{'metric':<24} {'A':>14} {'B':>14} {'∆':>14}")
for k in keys:
    av, bv = a.get(k, "-"), b.get(k, "-")
    try:
        delta = f"{int(bv)-int(av):+,}"
    except (TypeError, ValueError):
        delta = "-"
    print(f"{k:<24} {av:>14} {bv:>14} {delta:>14}")
PY
