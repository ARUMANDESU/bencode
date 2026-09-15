#!/usr/bin/env bash
set -euo pipefail

# # What does this script do?
# run benchmark tests and store everything in $BENCH_DIR

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
# BENCH_DIR: use env var if set, otherwise default
BENCH_DIR="${BENCH_DIR:-$REPO_ROOT/bench_results}"
mkdir -p "$BENCH_DIR"

TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
GIT_COMMIT="$(git rev-parse --short HEAD)"
OUT_FILE="$BENCH_DIR/bench_${TIMESTAMP}_${GIT_COMMIT}.txt"
BENCH_COUNT="${BENCH_COUNT:-1}"

echo "Running Go benchmarks..." >&2
echo "Repo root:   $REPO_ROOT" >&2
echo "Git commit hash: $GIT_COMMIT" >&2
echo "Output file: $OUT_FILE" >&2

cd "$REPO_ROOT"

go test ./... \
  -run='^$' \
  -bench=. \
  -benchmem \
  -count=$BENCH_COUNT |
  tee "$OUT_FILE"

echo >&2
echo "Done. Results saved at: $OUT_FILE" >&2
