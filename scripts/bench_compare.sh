#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

BENCH_DIR="${BENCH_DIR:-$REPO_ROOT/bench_results}"

if ! command -v benchstat >/dev/null 2>&1; then
  echo "benchstat not found. Install with:" >&2
  echo "  go install golang.org/x/perf/cmd/benchstat@latest" >&2
  exit 1
fi

if [ "$#" -ge 1 ]; then
  # Explicit files/args passed through as-is
  benchstat "$@"
else
  # Default: compare the two most recent results in BENCH_DIR
  mapfile -t FILES < <(ls -1t "$BENCH_DIR"/bench_*.txt 2>/dev/null | head -n 2)
  if [ "${#FILES[@]}" -lt 2 ]; then
    echo "Need at least 2 result files in $BENCH_DIR to compare." >&2
    echo "Found: ${#FILES[@]}" >&2
    exit 1
  fi
  # benchstat wants old-first, new-second; ls -t is newest-first, so reverse
  echo "Comparing:" >&2
  echo "  old: ${FILES[1]}" >&2
  echo "  new: ${FILES[0]}" >&2
  benchstat "${FILES[1]}" "${FILES[0]}"
fi
