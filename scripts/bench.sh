#!/usr/bin/env bash
# Thin wrapper so `scripts/bench.sh --label gpu --gpu-sample` works from the repo root.
set -euo pipefail
cd "$(dirname "$0")/.."
exec python3 scripts/bench.py "$@"
