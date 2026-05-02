#!/usr/bin/env bash
# Extracts top-30 cumulative text reports from each profile binary so the
# diagnosis markdowns can quote them without needing to re-run pprof.
#
# Run after run-profiles.sh has produced the .prof / .mem files.

set -euo pipefail

REPO=$(git rev-parse --show-toplevel)
OUT="$REPO/bench/profiles/${OUT_LABEL:-diagnosis-baseline}"

cpu_top()   { go tool pprof -top -cum -nodecount=30 "$1" 2>/dev/null; }
alloc_top() { go tool pprof -top -cum -sample_index="$2" -nodecount=30 "$1" 2>/dev/null; }

for label in inproc-hset tcp-hset-std inproc-get tcp-get-std inproc-set tcp-set-std tcp-get-pipe tcp-hset-pipe; do
    [[ -s "$OUT/cpu/$label.prof"   ]] && cpu_top   "$OUT/cpu/$label.prof"   > "$OUT/cpu/$label.top.txt"
    [[ -s "$OUT/alloc/$label.mem"  ]] && alloc_top "$OUT/alloc/$label.mem"  alloc_objects > "$OUT/alloc/$label.objects.txt"
    [[ -s "$OUT/alloc/$label.mem"  ]] && alloc_top "$OUT/alloc/$label.mem"  alloc_space   > "$OUT/alloc/$label.space.txt"
    [[ -s "$OUT/block/$label.prof" ]] && cpu_top   "$OUT/block/$label.prof" > "$OUT/block/$label.top.txt"
    [[ -s "$OUT/mutex/$label.prof" ]] && cpu_top   "$OUT/mutex/$label.prof" > "$OUT/mutex/$label.top.txt"
done

echo "Reports written under $OUT."
ls "$OUT/cpu/"*.top.txt "$OUT/alloc/"*.objects.txt "$OUT/block/"*.top.txt "$OUT/mutex/"*.top.txt 2>/dev/null
