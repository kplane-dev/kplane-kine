# kplane-kine

Run the [kplane](https://kplane.dev) apiserver with [kine](https://github.com/k3s-io/kine) as the storage shim, swapping etcd for a relational backend. **Postgres is the first / reference backend**; other backends (mysql, sqlite, nats) slot in alongside it under `manifests/backends/`.

## Status

**Under construction.** This repo is being built one PR at a time, with the design ratchet documented at each step. See [`docs/`](./docs) for the architecture and research notes, and the PR history for incremental commits.

When v1 is complete the repo will give you:
- A local `kind` cluster running kplane-apiserver against kine→postgres.
- A parallel kplane-apiserver running against vanilla etcd, same node, same config.
- A benchmark harness that runs identical workloads against both and prints a side-by-side comparison.
- A walkthrough README + automated CI that runs the bench on every PR.

## Why

Postgres is operationally much friendlier than etcd — your team already runs it, backups are trivial, replication is a solved problem, and the observability story is mature. Kine bridges the gap by exposing an etcd-compatible gRPC API on top of a SQL database, so the apiserver doesn't know the difference.

The unknown — and the thing this repo is built to measure honestly — is **what throughput you give up in exchange for that operational ergonomics**.

## Repo layout (target)

```
manifests/
  base/                  # kine + apiserver (backend-agnostic)
  backends/
    postgres/            # v1
    mysql/               # future
    sqlite/              # future
bench/                   # Go benchmark harness; runs against any backend
docs/
  research.md            # how kine works, how the apiserver uses it, expected perf shape
  architecture.md        # component diagram, version pins, kind cluster spec
  walkthrough.md         # step-by-step manual run
  results.md             # checked-in baseline numbers + methodology
scripts/                 # up.sh, down.sh, bench.sh, results.sh
```

## Quickstart (target, not yet wired)

```bash
make up         # kind cluster + postgres + kine + 2 apiservers (kine-backed and etcd-backed)
make bench      # run the side-by-side comparison
make results    # render the markdown report
make down
```

## Background

- **kine**: <https://github.com/k3s-io/kine> — "etcdshim that translates etcd API to SQLite/Postgres/MySQL/NATS." Originally built for k3s; widely deployed in single-cluster setups.
- **kplane apiserver**: an extension of the upstream `kube-apiserver` with a multicluster keyspace (`/registry/{resource}/clusters/{clusterID}/...`) for hosting many virtual control planes in one storage backend.

## License

Apache-2.0. See [`LICENSE`](./LICENSE).
