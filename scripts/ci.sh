#!/usr/bin/env bash
# What CI runs on every PR. Same script as `make ci` locally — if it
# passes on your laptop, it passes in GitHub Actions.
#
# Each commit replaces one placeholder with a real step.
# Current state: stages up/down. Bench wires in alongside feat(bench).

# shellcheck source=scripts/lib.sh
source "$(dirname "$0")/lib.sh"

cd "$REPO_ROOT" || die "could not cd to $REPO_ROOT"

log "ci.sh — preflight"
check_tool kind
check_tool kubectl
check_tool shellcheck   # CI parity: local + GH Actions both lint
check_tool go
check_tool jq

cleanup() {
  local rc=$?
  log "ci.sh — cleanup (final rc=$rc)"
  if [ "$rc" -ne 0 ]; then
    # Dump cluster state BEFORE teardown so the CI logs / local stderr
    # have something to look at when something failed mid-way.
    log "  capturing diagnostics before teardown"
    kubectl get pods -A 2>&1 || true
    for ns in kine-pg etcd-baseline; do
      kubectl get pods -n "$ns" -o wide 2>&1 || true
      kubectl describe pods -n "$ns" 2>&1 || true
      kubectl get events -n "$ns" --sort-by=.lastTimestamp 2>&1 | tail -30 || true
    done
  fi
  scripts/down.sh || true
  exit "$rc"
}
trap cleanup EXIT

# 1. lint
log "step: lint — shellcheck scripts/"
shellcheck scripts/*.sh
log "step: lint — go vet ./..."
go vet ./...

# 2. build binaries
log "step: build bench + e2e"
mkdir -p .local/bin
go build -o .local/bin/bench ./bench/cmd/bench
go build -o .local/bin/e2e   ./e2e/cmd/e2e

# 3. spin up cluster + manifests
log "step: up"
scripts/up.sh

# 4. e2e (correctness) — runs first, fail-fast before the bench wastes
# minutes producing numbers we can't trust.
log "step: e2e against both backends"
.local/bin/e2e --endpoint=https://localhost:6443 --certs=.local/certs/kine-pg       --backend=kine-postgres
.local/bin/e2e --endpoint=https://localhost:6444 --certs=.local/certs/etcd-baseline --backend=etcd

# 5. bench (throughput) — small ops count for CI; bump OPS for real numbers
log "step: bench"
OPS="${OPS:-50}" CONCURRENCY="${CONCURRENCY:-4}" scripts/bench.sh

# 6. render results so CI artifacts include a readable summary
log "step: results"
scripts/results.sh > .local/bench/SUMMARY.md
log "  → .local/bench/SUMMARY.md"
cat .local/bench/SUMMARY.md

# down handled by trap
log "ci.sh — done"
