#!/usr/bin/env bash
# Evidence-grade benchmark run wrapper (FR-010).
set -euo pipefail

echo "=== Evidence-Grade Benchmark Run ==="
echo "Go version: $(go version)"
echo "Kernel: $(uname -r)"
echo "Platform: $(uname -m)"
echo "CPU: $(grep 'model name' /proc/cpuinfo | head -1 | cut -d: -f2 | xargs)"
echo "GOMAXPROCS: $(nproc)"

if command -v cpupower >/dev/null 2>&1; then
    sudo -n cpupower frequency-set -g performance 2>/dev/null && echo "CPU governor: performance" || echo "CPU governor: FAILED (need root)"
else
    echo "CPU governor: cpupower not available"
fi

exec "$(dirname "$0")/run-threshold-validation.sh"
