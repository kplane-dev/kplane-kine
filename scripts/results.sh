#!/usr/bin/env bash
# Render the JSON results in .local/bench/ into a markdown table on
# stdout. Redirect into docs/results.md to persist a baseline:
#
#   ./scripts/results.sh > docs/results.md

# shellcheck source=scripts/lib.sh
source "$(dirname "$0")/lib.sh"
check_tool jq

RESULTS_DIR="$REPO_ROOT/.local/bench"

shopt -s nullglob
files=("$RESULTS_DIR"/*.json)
[ "${#files[@]}" -gt 0 ] || die "no results in $RESULTS_DIR — run 'make bench' first"

# Slurp all files into one array; group by workload; emit one table
# per workload with both backends side-by-side.
jq -s '
  group_by(.workload)
  | map({
      workload: .[0].workload,
      rows: (sort_by(.backend) | map({
        backend: .backend,
        ops_per_sec: (.ops_per_sec | (. * 10 | round) / 10),
        p50: .latency_ms.p50,
        p90: .latency_ms.p90,
        p99: .latency_ms.p99,
        errors: .errors,
      }))
    })
' "${files[@]}" | jq -r '
  .[] |
  "### " + (.workload | ascii_upcase) + "\n",
  "| backend | ops/sec | p50 (ms) | p90 (ms) | p99 (ms) | errors |",
  "|---|---:|---:|---:|---:|---:|",
  (.rows[] | "| " + .backend + " | " + (.ops_per_sec|tostring) + " | " + (.p50|tostring) + " | " + (.p90|tostring) + " | " + (.p99|tostring) + " | " + (.errors|tostring) + " |"),
  ""
'
