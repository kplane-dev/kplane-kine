#!/usr/bin/env bash
# Run the bench harness against both backends, sequentially. Writes one
# JSON file per (backend × workload) combination to .local/bench/.
#
# Sequential is load-bearing: both apiservers live on the same kind
# node, so running them in parallel would have them contend for CPU
# and disk and we'd measure contention, not backend characteristics.
#
# Env knobs (all optional):
#   OPS         number of ops per workload      (default 200)
#   CONCURRENCY parallel workers per workload   (default 8)
#   CPS         comma-separated control planes  (default cp-a,cp-b,cp-c)
#   PAYLOAD     bytes of ConfigMap payload      (default 4096)

# shellcheck source=scripts/lib.sh
source "$(dirname "$0")/lib.sh"

OPS="${OPS:-200}"
CONCURRENCY="${CONCURRENCY:-8}"
CPS="${CPS:-cp-a,cp-b,cp-c}"
PAYLOAD="${PAYLOAD:-4096}"

BENCH_BIN="$REPO_ROOT/.local/bin/bench"
RESULTS_DIR="$REPO_ROOT/.local/bench"

mkdir -p "$RESULTS_DIR"

# Build the bench binary if missing or stale (any .go file newer than it).
build_bench=1
if [ -x "$BENCH_BIN" ]; then
  build_bench=0
  while IFS= read -r f; do
    if [ "$f" -nt "$BENCH_BIN" ]; then
      build_bench=1
      break
    fi
  done < <(find "$REPO_ROOT/bench" -name '*.go')
fi
if [ "$build_bench" = "1" ]; then
  log "building bench binary"
  ( cd "$REPO_ROOT" && go build -o "$BENCH_BIN" ./bench/cmd/bench )
fi

run_one() {
  local backend="$1" endpoint="$2" certs="$3" workload="$4"
  local out="$RESULTS_DIR/$backend-$workload.json"
  log "running: backend=$backend workload=$workload ops=$OPS conc=$CONCURRENCY"
  "$BENCH_BIN" \
    --endpoint="$endpoint" \
    --certs="$certs" \
    --backend="$backend" \
    --workload="$workload" \
    --ops="$OPS" \
    --concurrency="$CONCURRENCY" \
    --cps="$CPS" \
    --payload-bytes="$PAYLOAD" \
    --out="$out" \
    >/dev/null
  log "  → $out"
}

# Cooldown between runs lets disk caches settle so we're measuring
# steady-state behavior, not the warm-cache halo of the previous run.
cooldown() {
  log "cooldown ${1:-5}s"
  sleep "${1:-5}"
}

# Order: kine first (the new thing under test), etcd second (baseline).
# CREATE then GET inside each backend so GET benefits from in-process
# create cache like a real workload would.
log "=== kine-pg backend ==="
run_one "kine-postgres" "https://localhost:6443" "$REPO_ROOT/.local/certs/kine-pg" "create"
cooldown
run_one "kine-postgres" "https://localhost:6443" "$REPO_ROOT/.local/certs/kine-pg" "get"

cooldown 10

log "=== etcd backend ==="
run_one "etcd" "https://localhost:6444" "$REPO_ROOT/.local/certs/etcd-baseline" "create"
cooldown
run_one "etcd" "https://localhost:6444" "$REPO_ROOT/.local/certs/etcd-baseline" "get"

log "all benchmark runs complete; results in $RESULTS_DIR/"
ls -1 "$RESULTS_DIR"
