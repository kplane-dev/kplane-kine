# Architecture

The system at v1: one `kind` node, two parallel storage stacks, one benchmark harness.

```
                            ┌─────────────────────────────────────┐
                            │              kind node              │
                            │                                     │
   bench job ───────────────┼──► apiserver-kine    ───► kine ───► │
   (in cluster, runs        │    (ns: kine-pg)                │   │
   workload twice,          │                                 ▼   │
   once per backend)        │                              postgres
                            │                                     │
                            │                                     │
                            │    apiserver-etcd    ────► etcd     │
                            │    (ns: etcd-baseline)              │
                            │                                     │
                            └─────────────────────────────────────┘
                                  identical CPU/memory budget per stack
                                  same `kplane-apiserver` image + flags
                                  only difference: --etcd-servers
```

Each stack lives in its own namespace so resource accounting and logs are easy to separate. Only one stack receives benchmark load at a time (see "Sequential benchmarking" below).

## Component versions (pin once, change deliberately)

| Component | Version | Image | Note |
|---|---|---|---|
| `kind` node | k8s 1.32+ | `kindest/node:v1.32.0` | Single-node. Multi-node adds no value here. |
| postgres | 16 | `postgres:16-alpine` | LTS. `-alpine` for image size. |
| kine | v0.15.0 | `ghcr.io/k3s-io/kine:v0.15.0` | Latest stable as of 2026-05-07. (`rancher/kine` on Docker Hub stopped at v0.14.14; v0.15+ ships only via GHCR.) |
| etcd | v3.5.x | `quay.io/coreos/etcd:v3.5.17` | Matches what kube-apiserver expects. |
| kplane-apiserver | `:latest` | `ghcr.io/kplane-dev/kplane-apiserver` | Private until repo goes public; see "Image access" below. |

Version pins live in `manifests/base/*.yaml` so a bump is a single commit + diff.

## Per-component sizing (justified)

Sized to fit on a 2-vCPU CI runner (GH Actions) with the kube-system overhead. Sum of requests must stay under ~1.3 cores to leave room for kube-apiserver / scheduler / kubelet in the kind node. Limits are generous so each pod can burst into available CPU under load — what matters for the benchmark is **headroom**, not **reservation**.

| Pod | CPU req | CPU limit | Mem req | Mem limit | Storage |
|---|---|---|---|---|---|
| postgres | 100m | 2000m | 128Mi | 1Gi | 5Gi PVC |
| kine | 50m | 1000m | 64Mi | 256Mi | — |
| etcd | 100m | 2000m | 128Mi | 1Gi | 5Gi PVC |
| apiserver (each) | 200m | 2000m | 256Mi | 1Gi | — |

Total requested: ~650m + kube-system (~500m) = ~1.15 cores reserved on a 2-core runner. Postgres and etcd carry symmetric envelopes by design — different requests would bias the head-to-head.

**Why low requests, high limits:** the benchmark measures backend behavior under burst, not steady-state at low CPU. Limits at 2 cores let each pod consume the whole runner when it's the active workload (we run backends sequentially), and the low requests let *every* pod schedule even when 2 apiservers + 2 storage pods are on the same 2-vCPU node.

## Kind cluster spec

```yaml
# manifests/kind/cluster.yaml (preview — lands in PR 3)
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: kplane-kine
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: 30443
        hostPort: 6443      # apiserver-kine NodePort
        protocol: TCP
      - containerPort: 30444
        hostPort: 6444      # apiserver-etcd NodePort
        protocol: TCP
    extraMounts:
      - hostPath: /tmp/kplane-kine-data
        containerPath: /var/lib/kplane-kine
        readOnly: false
```

Two host ports so `bench` running on the laptop (not in-cluster) can hit both apiservers directly. The mount gives postgres/etcd a real host-disk PVC backend — kind's default emptyDir would skew write perf (no fsync to a real disk).

## Postgres config

Shipped as a ConfigMap mounted into the postgres pod:

```
shared_buffers = 128MB        # 25% of mem request, standard guidance
wal_buffers = 8MB
effective_cache_size = 384MB
synchronous_commit = on       # default; we explicitly pin so it's visible in the diff
wal_level = replica
max_connections = 100
log_min_duration_statement = 100ms  # surfaces slow queries in pod logs
```

**Explicitly NOT set:**
- `synchronous_commit = off` — we want a fsync-real comparison. A future PR can publish an `off` variant in `docs/results.md` for completeness.
- `fsync = off` — would invalidate the comparison entirely.
- Aggressive `shared_buffers` (>1GB) — the pod doesn't have the memory and we'd be measuring page cache, not the backend.

## Kine config

CLI flags:
```
kine \
  --listen-address=0.0.0.0:2379 \
  --endpoint=postgres://kine:${PG_PASSWORD}@postgres.kine-pg.svc:5432/kine?sslmode=disable \
  --metrics-bind-address=0.0.0.0:8080
```

- `--poll-interval` defaults to 1s; we'll publish a 100ms variant for the watch benchmark separately.
- Connection pool size: we'll set `KINE_DB_CONN_POOL_MAX=50` via env to remove the default-5 bottleneck under load.

## kplane-apiserver flags

Same flags for both deployments except `--etcd-servers`:

```
kplane-apiserver \
  --bind-address=0.0.0.0 \
  --secure-port=6443 \
  --etcd-servers=http://kine.kine-pg.svc:2379    # OR  http://etcd.etcd-baseline.svc:2379
  --etcd-prefix=/registry \
  --service-cluster-ip-range=10.0.0.0/16 \
  --tls-cert-file=/etc/certs/apiserver.crt \
  --tls-private-key-file=/etc/certs/apiserver.key \
  --client-ca-file=/etc/certs/ca.crt \
  --service-account-key-file=/etc/certs/sa.pub \
  --service-account-signing-key-file=/etc/certs/sa.key \
  --service-account-issuer=https://kplane-kine.local
```

No `--etcd-certfile`/`--etcd-keyfile`/`--etcd-cafile` — neither kine nor in-cluster etcd uses client mTLS in this setup. Network isolation (single namespace per stack, NetworkPolicy in v1.5) carries the security weight.

## Sequential benchmarking

The bench harness runs each workload **against one backend at a time** — never both in parallel on the same node. Reason: both apiservers and their backends would contend for the same node CPU/disk, and the comparison would measure contention, not backend characteristics.

Concretely:
1. Run workload `X` against `apiserver-kine` for `--ops` ops. Record results.
2. Pause for `--cooldown` (default 30s) so disk caches settle.
3. Run workload `X` against `apiserver-etcd` for `--ops` ops. Record results.
4. Move to the next workload.

The `bench/run.sh` wrapper orchestrates this; manifests for `bench` deploy it as a `Job` that runs sequentially through the workload list.

## Image access

Until this repo goes public:
- `ghcr.io/kplane-dev/kplane-apiserver` is private. Local-laptop pulls work because you're logged in to GHCR.
- CI (when added in PR 11) uses a `KPLANE_GHCR_TOKEN` secret to pull.

When the repo goes public:
- Either publish a public `kplane-apiserver` image (preferred — most reproducible), or
- Default the manifests to upstream `registry.k8s.io/kube-apiserver` and document that the kine→postgres value applies equally there, with a `--apiserver=kplane` opt-in for users with GHCR access.

This decision can wait until PR 12.

## What's NOT in v1

- Multi-shard apiserver (one shard, one storage stack each side).
- HA postgres / etcd (single node).
- Cross-region / multi-zone (single kind node, by design).
- Custom CRDs from kplane (the bench uses a generic CRD shipped in this repo).
- Sustained-load / soak testing (workloads run 30s-5min).
- Real TLS to the backend (kine doesn't speak client mTLS).
- Network policy / Pod Security Standards (out of scope; would land in v1.5 alongside the public release).
