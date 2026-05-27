#!/usr/bin/env bash
# Tear down the kind cluster created by scripts/up.sh.
# Idempotent: silent if the cluster doesn't exist.

# shellcheck source=scripts/lib.sh
source "$(dirname "$0")/lib.sh"

check_tool kind

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  log "deleting kind cluster: $CLUSTER_NAME"
  kind delete cluster --name "$CLUSTER_NAME"
else
  log "no kind cluster named $CLUSTER_NAME — nothing to delete"
fi
