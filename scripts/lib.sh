# shellcheck shell=bash
# Shared helpers for the kplane-kine scripts. Source from each entrypoint:
#   # shellcheck source=scripts/lib.sh
#   source "$(dirname "$0")/lib.sh"
# Provides:
#   - log + die functions with consistent timestamps
#   - REPO_ROOT, CLUSTER_NAME defaults
#   - check_tool helper

set -euo pipefail

# shellcheck disable=SC2034  # used by sourcing scripts via $REPO_ROOT

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-kplane-kine}"

log() {
  printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" >&2
}

die() {
  printf '[%s] ERROR: %s\n' "$(date +%H:%M:%S)" "$*" >&2
  exit 1
}

check_tool() {
  local tool="$1"
  command -v "$tool" >/dev/null 2>&1 || die "required tool not on PATH: $tool"
}
