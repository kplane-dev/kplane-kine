#!/usr/bin/env bash
# Spin up the kplane-kine demo cluster.
#
# Stages (added by successive commits — each stage is idempotent):
#   1. kind cluster                                     [done]
#   2. postgres                                         [done]
#   3. kine pointed at postgres                         [done]
#   4. kplane-apiserver (kine-backed)                   [done]
#   5. etcd + apiserver (etcd-backed)                   [this commit]
#
# Re-running on an existing cluster is a no-op for the cluster step;
# later stages reapply manifests in place.

# shellcheck source=scripts/lib.sh
source "$(dirname "$0")/lib.sh"

check_tool kind
check_tool kubectl

KIND_CONFIG="$REPO_ROOT/kind/cluster.yaml"

# --- stage 1: kind cluster ------------------------------------------------

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  log "kind cluster '$CLUSTER_NAME' already exists — skipping creation"
else
  log "creating kind cluster '$CLUSTER_NAME'"
  kind create cluster --config "$KIND_CONFIG" --wait 60s
fi

log "kubectl context: $(kubectl config current-context 2>/dev/null || echo '?')"
log "nodes:"
kubectl get nodes -o wide

# --- stage 2: postgres ----------------------------------------------------

log "applying postgres manifests (kine-pg namespace)"
kubectl apply -k "$REPO_ROOT/manifests/backends/postgres"

log "waiting for postgres-0 Ready"
# StatefulSet needs to create the pod before we can wait on it; the
# rollout-status flavor handles both the pre-create and post-Ready window.
kubectl rollout status statefulset/postgres -n kine-pg --timeout=300s

log "smoke-testing postgres connection"
kubectl exec -n kine-pg postgres-0 -- pg_isready -U kine -d kine
kubectl exec -n kine-pg postgres-0 -- psql -U kine -d kine -c "SELECT version();"

# --- stage 3: kine ---------------------------------------------------------
# Already applied above (kine-deployment.yaml + kine-service.yaml are part
# of the same kustomization as postgres). Wait for it Ready + smoke test.

log "waiting for kine Ready"
kubectl rollout status deployment/kine -n kine-pg --timeout=180s

log "smoke-testing kine via etcd protocol (put/get round-trip through kine)"
# Use a one-shot etcd-client pod to exercise the etcd gRPC interface kine
# exposes. If kine doesn't faithfully implement etcd's PUT/GET, this fails
# — and the whole "kplane apiserver thinks it's talking to etcd" story
# would break in the next checkpoint.
#
# coreos/etcd is distroless (no shell), so we run etcdctl directly in
# separate invocations. We deliberately skip standalone `del` — kine
# returns Unimplemented for that, since the apiserver does deletes via
# Txn(DeleteRange), not bare Delete. The bench + e2e exercise the real
# delete path.
ETCD_IMG=quay.io/coreos/etcd:v3.5.17

# Capture-then-test (no pipes mid-stream) so kubectl's "pod deleted"
# cleanup print doesn't SIGPIPE a downstream grep and trip pipefail.
put_out=$(kubectl run kine-smoke-put --rm -i --restart=Never --image="$ETCD_IMG" \
  --env=ETCDCTL_API=3 --env=ETCDCTL_ENDPOINTS=http://kine.kine-pg.svc:2379 \
  --command -- etcdctl put /smoke/hello world 2>&1)
case "$put_out" in
  *OK*) : ;;
  *)    die "kine PUT failed: $put_out" ;;
esac

get_out=$(kubectl run kine-smoke-get --rm -i --restart=Never --image="$ETCD_IMG" \
  --env=ETCDCTL_API=3 --env=ETCDCTL_ENDPOINTS=http://kine.kine-pg.svc:2379 \
  --command -- etcdctl get /smoke/hello --print-value-only 2>&1)
# get_out has "world\n<pod deleted message>" — pick first line
got=$(printf '%s\n' "$get_out" | head -n1 | tr -d '[:space:]')
if [ "$got" != "world" ]; then
  die "kine GET mismatch: expected 'world', got '$got' (full: $get_out)"
fi
log "kine smoke OK (etcd put/get round-trip through kine→postgres)"

# bootstrap_apiserver: shared helper that deploys an apiserver into a
# namespace and runs a CRUD + isolation smoke test against it. Used
# for both the kine-pg stack (stage 4) and the etcd-baseline stack
# (stage 5) so the two stacks are bit-identical in everything except
# the backend.
#
# Args:
#   $1 = label  (human name, e.g. "kine-pg" / "etcd-baseline")
#   $2 = ns
#   $3 = kustomize path
#   $4 = host-port for the apiserver NodePort (e.g. 6443, 6444)
bootstrap_apiserver() {
  local label="$1" ns="$2" kustomize_dir="$3" host_port="$4"
  local certs_dir="$REPO_ROOT/.local/certs/$ns"
  local url="https://localhost:$host_port"

  log "$label: generating apiserver certs at $certs_dir"
  "$REPO_ROOT/scripts/gen-certs.sh" "$certs_dir" \
    kplane-apiserver \
    "kplane-apiserver.$ns" \
    "kplane-apiserver.$ns.svc" \
    "kplane-apiserver.$ns.svc.cluster.local" \
    localhost \
    127.0.0.1

  # Namespace may not exist yet (etcd-baseline case) — apply manifests first
  # to create it, then push the Secrets into the namespace and rollout.
  log "$label: applying manifests"
  kubectl apply -k "$kustomize_dir"

  log "$label: creating apiserver Secrets in $ns"
  kubectl create secret generic kplane-apiserver-tls \
    --from-file=tls.crt="$certs_dir/tls.crt" \
    --from-file=tls.key="$certs_dir/tls.key" \
    --from-file=ca.crt="$certs_dir/ca.crt" \
    -n "$ns" --dry-run=client -o yaml | kubectl apply -f -
  kubectl create secret generic kplane-apiserver-sa \
    --from-file=sa.key="$certs_dir/sa.key" \
    --from-file=sa.pub="$certs_dir/sa.pub" \
    -n "$ns" --dry-run=client -o yaml | kubectl apply -f -
  kubectl create secret generic kplane-apiserver-token-auth \
    --from-file=token.csv="$certs_dir/token.csv" \
    -n "$ns" --dry-run=client -o yaml | kubectl apply -f -

  # Re-apply so the Deployment picks up newly-created Secrets (in case
  # the first apply happened before Secrets existed).
  kubectl rollout restart deployment/kplane-apiserver -n "$ns" >/dev/null 2>&1 || true

  log "$label: waiting for kplane-apiserver Ready"
  kubectl rollout status deployment/kplane-apiserver -n "$ns" --timeout=300s

  local token; token=$(cat "$certs_dir/admin.token")

  log "$label: /healthz check"
  for i in 1 2 3 4 5 6 7 8 9 10; do
    if curl -fsSk -H "Authorization: Bearer $token" "$url/healthz" >/dev/null; then
      log "  $label: /healthz OK"
      break
    fi
    sleep 2
    [ "$i" = 10 ] && die "$label: /healthz never returned 200"
  done

  log "$label: multi-CP CRUD smoke (cp-a/smoke)"
  # The apiserver auto-bootstraps the default 'kubernetes' Service in
  # each virtual CP on first access (--kubernetes-service-mode=
  # per-cluster-autoip). First request can race that bootstrap and
  # return 500. Retry briefly until the CP is ready to accept writes.
  curl -fsSk -X DELETE -H "Authorization: Bearer $token" \
    "$url/clusters/cp-a/control-plane/api/v1/namespaces/default/configmaps/smoke" \
    >/dev/null 2>&1 || true
  local body='{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"smoke","namespace":"default"},"data":{"k":"v"}}'
  local created=""
  for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    if curl -fsSk -X POST -H "Authorization: Bearer $token" \
         -H "Content-Type: application/json" --data "$body" \
         "$url/clusters/cp-a/control-plane/api/v1/namespaces/default/configmaps" \
         >/dev/null 2>&1; then
      created=ok
      break
    fi
    sleep 2
  done
  [ "$created" = "ok" ] || die "$label: multi-CP smoke FAILED — cp-a create never succeeded after 30s of retries"

  local got
  got=$(curl -fsSk -H "Authorization: Bearer $token" \
    "$url/clusters/cp-a/control-plane/api/v1/namespaces/default/configmaps/smoke" \
    | grep -oE '"k":[[:space:]]*"v"' | head -n1)
  [ -n "$got" ] || die "$label: multi-CP smoke FAILED — data.k=v not found in cp-a/smoke"
  log "  $label: multi-CP CRUD OK (cp-a/smoke roundtrip)"

  log "$label: multi-CP isolation (cp-b/smoke → expect 404)"
  local iso_code
  iso_code=$(curl -sk -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $token" \
    "$url/clusters/cp-b/control-plane/api/v1/namespaces/default/configmaps/smoke")
  [ "$iso_code" = "404" ] || die "$label: ISOLATION FAILED — cp-b/smoke returned $iso_code, expected 404"
  log "  $label: multi-CP isolation OK (cp-b/smoke → 404)"
}

# --- stage 4: kplane-apiserver (kine-backed) ------------------------------

bootstrap_apiserver "kine-pg" "kine-pg" "$REPO_ROOT/manifests/backends/postgres" 6443

# --- stage 5: etcd + apiserver (etcd-backed) ------------------------------

log "etcd-baseline: applying etcd manifests"
kubectl apply -k "$REPO_ROOT/manifests/etcd-baseline"

log "etcd-baseline: waiting for etcd Ready"
kubectl rollout status statefulset/etcd -n etcd-baseline --timeout=300s

bootstrap_apiserver "etcd-baseline" "etcd-baseline" "$REPO_ROOT/manifests/etcd-baseline" 6444

# --- both stacks live; one storage backend each, identical apiserver -----

log "up.sh — both stacks live: kine-pg @ :6443 (postgres), etcd-baseline @ :6444 (etcd)"
