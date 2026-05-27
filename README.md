# kplane-kine

Run the [kplane](https://kplane.dev) apiserver with [kine](https://github.com/k3s-io/kine) as the storage shim, swapping etcd for a relational backend. **Postgres is the first / reference backend**; other backends (mysql, sqlite, nats) slot in alongside it under `manifests/backends/`.

## Why

Postgres is operationally much friendlier than etcd — your team already runs it, backups are trivial, replication is a solved problem, and the observability story is mature. Kine bridges the gap by exposing an etcd-compatible gRPC API on top of a SQL database, so the apiserver doesn't know the difference.

The unknown — and the thing this repo measures honestly — is **what throughput you give up in exchange for that operational ergonomics**.

## Quickstart

```bash
make up         # kind cluster + postgres + kine + apiserver-kine + etcd + apiserver-etcd
make bench      # run the side-by-side benchmark, writes JSON to .local/bench/
make results    # render the JSON into a markdown table on stdout
make down       # tear down the kind cluster
make ci         # run the full pipeline locally (what GitHub Actions runs on PRs)
```

Prereqs: `docker`, `kind`, `kubectl`, `go` 1.23+, `jq`, `shellcheck`, `openssl`.

What you get after `make up`:

- **kine-postgres stack** in namespace `kine-pg` — apiserver reachable at `https://localhost:6443` (multi-CP URLs: `https://localhost:6443/clusters/cp-a/control-plane/...`).
- **etcd baseline stack** in namespace `etcd-baseline` — apiserver at `https://localhost:6444`. Same image, same flags, only `--etcd-servers` differs.

Three "virtual control planes" (`cp-a`, `cp-b`, `cp-c`) are addressable via the URL prefix on each apiserver. Each is its own keyspace partition; create/list/watch ops in one don't leak into another.

## What's in here

- **End-to-end correctness suite** (`e2e/cmd/e2e/`): CRUD, LIST scope, WATCH ordering, WATCH isolation — all across three control planes. Run against both backends in every CI build.
- **Bench harness** (`bench/cmd/bench/`): CREATE + GET workloads (more in follow-up PRs), throughput + p50/p90/p99 latency, per-CP op counts, JSON output.
- **CI workflow** that runs the **exact same script** as `make ci` locally — if your laptop's green, GH Actions is green.

## Results so far

Hardware matters — the same code shows different ratios on different machines. Here's what we've measured.

**Local (Mac, 16 cores, 50 ops × 4 workers):**

| Workload | Backend | ops/sec | p50 (ms) | p99 (ms) |
|---|---|---:|---:|---:|
| CREATE | etcd | 1140 | 2.9 | 8.3 |
| CREATE | kine-postgres | 750 | 3.4 | 19.9 |
| GET | etcd | 4675 | 0.8 | 1.2 |
| GET | kine-postgres | 1547 | 1.4 | 8.2 |

**CI (Azure 2-vCPU runner, same workload):**

| Workload | Backend | ops/sec | p50 (ms) | p99 (ms) |
|---|---|---:|---:|---:|
| CREATE | etcd | 514 | 7.2 | 13.0 |
| CREATE | kine-postgres | 155 | 12.6 | 96.1 |
| GET | etcd | 1010 | 3.5 | 6.5 |
| GET | kine-postgres | 314 | 6.5 | 91.8 |

**Reading these honestly:**
- **Throughput**: etcd is 1.5–3× faster on CREATE, 3× faster on GET. The gap widens under CPU contention (CI) — kine→postgres has more sequential CPU per op (SQL hop, WAL fsync, prefix scan vs in-memory mvcc).
- **Tail latency**: etcd p99 is 2–7× tighter on local, up to 14× tighter under CI contention. This is the most consistent signal because reads are the cheapest op — any extra hop shows up cleanly without being smeared by write amplification or fsync variance.
- **Watch latency** (not in the table): kine's default poll interval is **1 second**, so first-event-after-write latency floors there vs. etcd's sub-millisecond push notifications. Tunable down to ~100ms at the cost of idle DB query load.

These numbers fall in the broadly-reported band for kine ([2–5× slower reads, 3–10× slower writes](https://github.com/k3s-io/kine)). Kine's own docs note it's "tested for k3s use, not a drop-in etcd replacement" — that holds up.

**When kine→postgres is the right call:** small or medium cluster, the team already runs postgres, p99 isn't load-bearing, and operational ergonomics (backups, replication, monitoring) outweighs the throughput cost.

**When it isn't:** hot multicluster apiserver, lots of watchers, tail latency is a customer-visible signal.

## Repo layout

```
manifests/
  backends/postgres/           # postgres + kine + kine-backed apiserver
  etcd-baseline/               # etcd + etcd-backed apiserver
bench/cmd/bench/               # Go benchmark harness
e2e/cmd/e2e/                   # multi-CP correctness suite (CRUD + LIST + WATCH)
docs/
  research.md                  # how kine + the kplane apiserver actually work
  architecture.md              # component versions, sizing, why each choice
kind/cluster.yaml              # single-node kind cluster spec
scripts/                       # up.sh, down.sh, bench.sh, results.sh, ci.sh, gen-certs.sh
.github/workflows/ci.yaml      # runs `make ci` on every push + PR
```

## Background

- **kine**: <https://github.com/k3s-io/kine> — "etcdshim that translates etcd API to SQLite/Postgres/MySQL/NATS." Originally built for k3s; widely deployed in single-cluster setups.
- **kplane apiserver**: an extension of the upstream `kube-apiserver` with a multicluster keyspace (`/registry/{resource}/clusters/{clusterID}/...`) for hosting many virtual control planes in one storage backend.

For the design ratchet — how kplane's apiserver uses storage, why kine should be drop-in compatible, what perf shape to expect — see [`docs/research.md`](./docs/research.md). For the topology choices, version pins, and sizing decisions, [`docs/architecture.md`](./docs/architecture.md).

## License

Apache-2.0. See [`LICENSE`](./LICENSE).
