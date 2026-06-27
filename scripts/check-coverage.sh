#!/usr/bin/env bash
# check-coverage.sh — fail if any package's test coverage drops below its floor.
# Guards against silent coverage regressions (audit #102 part 3). Wire this into
# CI (#87) alongside `go test -race`, gofmt, go vet, and govulncheck.
#
# Usage: scripts/check-coverage.sh
# Portable to bash 3.2 (macOS default) — no associative arrays.
set -euo pipefail

# Per-package floors (percent). Set a few points below the current level so normal
# churn passes but a real regression fails. Untested entrypoints (main) are skipped.
floor_for() {
  case "$1" in
    */cmd)             echo 65 ;;
    */internal/core)   echo 70 ;;
    */internal/hl)     echo 70 ;;
    */internal/state)  echo 70 ;;
    */internal/config) echo 75 ;;
    */internal/output) echo 85 ;;
    */internal/wallet) echo 85 ;;
    *)                 echo 70 ;; # default floor
  esac
}

fail=0
# `go test -cover` prints: ok  <pkg>  <elapsed>  coverage: NN.N% of statements
while IFS= read -r line; do
  case "$line" in
    *"coverage:"*)
      pkg=$(awk '{print $2}' <<<"$line")
      # Only gate subpackages; skip the bare module root (the main entrypoint,
      # intentionally untested) and any non-"ok" line shape.
      case "$pkg" in
        github.com/erickuhn19/deliverator/*) ;;
        *) continue ;;
      esac
      pct=$(sed -E 's/.*coverage: ([0-9.]+)% of statements.*/\1/' <<<"$line")
      floor=$(floor_for "$pkg")
      pct10=$(awk -v p="$pct" 'BEGIN{printf "%d", p*10}')   # integer compare (no floats in sh)
      flr10=$((floor * 10))
      if [ "$pct10" -lt "$flr10" ]; then
        printf 'FAIL  %-45s %5s%% < %d%% floor\n' "$pkg" "$pct" "$floor"
        fail=1
      else
        printf 'ok    %-45s %5s%% (>= %d%%)\n' "$pkg" "$pct" "$floor"
      fi
      ;;
  esac
done < <(go test ./... -cover -count=1 2>/dev/null)

if [ "$fail" -ne 0 ]; then
  echo "coverage floor breached — add tests or justify lowering the floor in scripts/check-coverage.sh" >&2
  exit 1
fi
echo "coverage floors OK"
