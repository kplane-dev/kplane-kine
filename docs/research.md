# Research: kplane-apiserver on kine + postgres

**Status:** PR 1 of the kplane-kine build-out. This doc grounds every following PR. It is intentionally code-cited rather than hand-wavy — if a claim doesn't have a file path or upstream link, it's marked **assumption**.

## Goal of this repo

Run the kplane apiserver against [kine](https://github.com/k3s-io/kine) (an etcd-shim that speaks the etcd gRPC API but stores data in a SQL backend), with postgres as the v1 backend. Quantify the throughput/latency tradeoff vs. vanilla etcd via a side-by-side benchmark on a single `kind` node.

The goal is **not** to prove kine beats etcd. It's to measure the cost honestly so a future operator can decide whether the operational savings (postgres tooling, well-understood backups, mature replication) are worth the perf delta for their workload.

---

## How kplane-apiserver uses storage today

Read directly from `/Users/zach/repos/kplane-dev/apiserver` and `/Users/zach/repos/kplane-dev/storage`:

### Stack
- **Standard upstream stack.** kplane-apiserver embeds `k8s.io/apiserver` and uses its `storage.Interface`. There's no custom etcd client; the wire-level etcd RPC is upstream's `go.etcd.io/etcd/client/v3` v3.6.7 (transitive via `k8s.io/apiserver`).
- The only kplane wrapping is `apiserver/pkg/multicluster/storage.go`, which decorates the `RESTOptionsGetter` to inject a key rewriter. It does **not** intercept transactions, watches, or compaction.

### Key layout
- Defined in `github.com/kplane-dev/storage/keylayout.go`. The layout is:
  ```
  /{etcd-prefix}/{resourcePath}/clusters/{clusterID}/{namespace}/{name}
  ```
- The `clusters/` segment is hard-coded as `ClusterSegment`. Helpers (`IdentityFromKey`, `ClusterFromKey`, `PerClusterPrefix`, `KindRootPrefix`) all work over byte-prefix operations on the key.
- **Implication for kine:** kine's backing table is keyed on `name`. Prefix scans (`Range`) on a long shared prefix translate to indexed range queries on the `name` column. Postgres handles this well with a B-tree on `(name, deleted, revision DESC)` (kine's default index set — confirmed below).

### Etcd-specific features in use
Checked exhaustively in the apiserver and storage packages:

| Feature | Used? | How / where |
|---|---|---|
| `Txn(If/Then/Else)` for compare-and-swap | Yes (indirect) | Standard upstream apiserver — `storage.Interface.GuaranteedUpdate` → upstream etcd3 driver issues `Txn`. Kplane passes through. **kine implements `Txn`.** |
| Etcd leases (`Lease`, `LeaseKeepAlive`) | No | Confirmed — no `Lease` calls in kplane code. Used only by upstream Event TTL, which can be disabled or run through kine's TTL goroutine. |
| Compaction | Acks but no kplane-side trigger | Upstream apiserver runs its own compaction loop. **kine acknowledges `Compact` RPCs but ignores them** — it runs internal compaction on a timer. |
| Watch progress notifications | Yes | `RequestWatchProgress` is forwarded through the apiserver. **kine implements `ProgressRequest`.** |
| `Range` queries (prefix scans) | Yes, heavily | All `LIST` and cross-cluster reads. **kine implements `Range`.** |
| mTLS to backend | Optional | Stripped in our setup — kine speaks plaintext etcd over TCP on a cluster-internal `ClusterIP` Service. Network isolation does the job. |

### Multicluster cross-cluster reads
`apiserver/pkg/multicluster/storage.go:321-332`: when `ResourceScopeCrossClusterRead` is set, the storage decorator issues a `Range` against `/{prefix}/{resource}/clusters/` (no clusterID in the key) so internal reconcilers can scan all clusters in one shot. This is a single prefix scan — well within kine's capabilities. Watch out for perf if any one resource type grows to millions of items (a full table scan over `name LIKE '/.../clusters/%'` is index-friendly but still O(N)).

### Sharding
The dormant `apiserver/pkg/multicluster/sharding/` code is **storage-agnostic** — it uses `Coordination.Lease` API objects, not etcd leases. Activating it later requires zero kine-specific changes.

### Verdict
**Drop-in compatible.** Swap the `--etcd-servers` flag from a real etcd endpoint to a kine endpoint and remove `--etcd-certfile`/`--etcd-keyfile`/`--etcd-cafile` (kine doesn't speak client mTLS). Network isolation must be considered when going to production but is irrelevant for the in-cluster benchmark.

---

## How kine works internally

Source: kine v0.15.0 (May 2026 release) plus `docs/flow.md` upstream. Confirmed by reading [k3s-io/kine on GitHub](https://github.com/k3s-io/kine).

### Single-table SQL design
A single row represents one revision of one key:

```
revision (auto-incrementing PK)
key
value          (current contents)
prev_value
prev_revision
create_flag
delete_flag
ttl
```

A `PUT` is an `INSERT` appending a new row. `DELETE` is an `INSERT` with `delete_flag=true`. There is **no in-place update** — every change is a new row. Compaction (covered below) prunes old rows.

### Strict revision monotonicity via "fill records"
Etcd guarantees revisions are dense and monotonic. Postgres's `SERIAL` auto-increment can leave gaps if a transaction aborts mid-insert (the sequence number is consumed and not returned). Kine compensates by inserting "fill records" — dummy rows whose only purpose is to occupy a revision that would otherwise be gap-filled later.

**Perf implication:** under high write concurrency, kine occasionally needs to issue extra inserts to keep revisions dense. This is one of the bigger reasons kine writes can be slower than etcd's — etcd's mvcc is a single-process append.

### Read path
- `Get(key)`: indexed lookup on `(name, deleted DESC, revision DESC)`.
- `Range(prefix, prefix+1)`: range scan on the same index.
- No mvcc-style in-memory store — every read is a SQL query. Cold reads pay round-trip + planner cost; warm reads benefit from postgres's buffer cache.

### Write path
1. `INSERT` the new row.
2. If a fill is needed, `INSERT` a fill row to close the gap.
3. Notify the in-process poll loop (via a 1024-revision buffered Go channel) that something happened.
4. Return success to the apiserver.

The `INSERT` is in a transaction that includes the fill insert, so it's atomic. Postgres's `wal_level=replica` and `synchronous_commit=on` (defaults) make each commit pay one WAL fsync.

### Watch path (this is where the perf shape diverges hardest)
Etcd watches are push-based: the mvcc store notifies subscribers directly when revisions advance.

Kine watches are **poll-based + broadcast fan-out**:
1. A single goroutine polls the database for new rows since the last seen revision.
2. New rows are converted to events and pushed into a broadcaster channel.
3. The broadcaster fans events out to subscriber channels (100-event buffer per subscriber, 100-event filter buffer for prefix-matching).
4. Each watch subscriber's gRPC server forwards events to the client.

**Practical implications:**
- Watch notification latency floor = poll interval (default 1s, configurable to tighter via `--poll-interval`).
- High event rate → subscribers can drop if the broadcaster buffer overflows.
- A heavy creator/watcher mix can hit broadcaster backpressure earlier than etcd would.

We will see this clearly in the benchmark's `WATCH thrash` workload.

### Compaction
- Kine runs internal compaction on a timer (default ~5 min).
- Old rows for the same key are physically deleted.
- Upstream etcd's `Compact` RPCs are **acknowledged but ignored** — the kplane apiserver's compactor calls them but they're no-ops; kine compacts on its own schedule.
- Postgres autovacuum picks up the deleted rows. Bloat is real and worth monitoring (`pg_stat_user_tables`).

### TTL / leases
A goroutine watches events and queues future deletions. Re-checks current TTL on dequeue (so leases extended after-the-fact aren't deleted prematurely). Looser than etcd's tightly-timed lease semantics but works for Events and Lease objects in practice.

---

## Expected perf shape (predictions, to be validated)

| Workload | Expectation | Why |
|---|---|---|
| Single-key `GET` (warm cache) | **~comparable** | Both are an indexed lookup; postgres buffer cache + apiserver in-memory watch cache hide the SQL cost. |
| Single-key `GET` (cold) | **2-5× slower on kine** | Postgres planner + buffer cache miss + WAL/redo bookkeeping. |
| `LIST` of 10k objects | **2-3× slower on kine** | Postgres prefix scan vs. etcd in-memory mvcc walk. |
| `CREATE` throughput | **3-10× slower on kine** | INSERT + fill row + WAL fsync per write. Postgres tuning matters here a lot. |
| `UPDATE` throughput | **3-10× slower on kine** | Same as CREATE — kine writes are append-only. |
| `DELETE` throughput | **3-10× slower on kine** | Tombstone insert + fill. |
| `WATCH` notification latency | **~1s floor** vs. **~ms on etcd** | Kine poll loop, default poll interval. |
| `WATCH` event rate ceiling | **lower on kine** | Broadcaster buffer pressure. |
| 50k mixed ops, 16 concurrent | **kine bottlenecks on writes** | The whole flow funnels through one connection's WAL fsync. |

The qualitative story will likely be: **reads are fine, writes are the cost, watches are the surprise.** That's what we should expect the bench to show. If reality looks dramatically different, that's a red flag in either kine or our methodology.

---

## Benchmark methodology

### Isolating the variable
- Same kind node, same node resources, same `kplane-apiserver` image + flags (except `--etcd-servers`), same CRD, same workload binary, same time window.
- Run benchmarks **sequentially**, not in parallel — running both apiservers simultaneously on the same node would have them contend for the same CPU/disk and the comparison would be useless.
- Warm-up: run a short discard pass before measuring, so the postgres buffer cache and etcd mvcc cache aren't cold for the first workload.

### Workloads
Five workloads, each parameterized by `--ops`, `--concurrency`, `--payload-bytes`:

1. **`create`** — INSERT N objects with random names + payload.
2. **`get`** — random GET against a pre-populated set.
3. **`list`** — full LIST of the entire pre-populated set, repeated N times.
4. **`update`** — random PATCH against the pre-populated set.
5. **`delete`** — DELETE all N objects.
6. **`mixed`** — 50% GET, 30% UPDATE, 20% CREATE, drawn randomly per worker.
7. **`watch`** — N watchers + N concurrent writers; measure event-delivery latency.

### Metrics reported (per workload, per backend)
- Throughput (ops/sec).
- Latency p50, p90, p99 (ms).
- Per-second timeline (for spotting backpressure spikes).
- Error count + error type breakdown.

### What we won't claim from this bench
- Production scale. Kind on a laptop is not a 1000-node cluster.
- Postgres at scale. We use sane defaults, not a tuned production rig.
- Long-running stability. We measure 30s–5min windows. Bloat, vacuum behavior, replication lag — all out of scope.
- Multi-shard kine throughput. v1 is one apiserver, one kine, one postgres.

These are explicit non-goals so the bench numbers can't be over-interpreted.

---

## Open questions

1. **Postgres tuning baseline.** We ship `shared_buffers=128MB`, `synchronous_commit=on`, `wal_level=replica`. Anyone benchmarking should be able to defend "this is sane out-of-the-box postgres, not tuned for or against the workload." Worth documenting whether a `synchronous_commit=off` variant materially closes the gap.
2. **Kine `--poll-interval`.** Default is 1s. Watch latency floor follows. Could drop to 100ms for a fairer apples-to-apples on watch — at the cost of higher idle DB query load. We'll publish both.
3. **Connection pooling.** kine→postgres uses a fixed connection pool. Default 5 conns. Concurrency above that bottlenecks at the SQL driver. We should bump this for the high-concurrency workloads and document.
4. **Etcd baseline config.** Single-node etcd is the apples-to-apples comparison for a single-node kine. A 3-node etcd cluster on the same node would be misleading; etcd quorum cost would be unfairly amortized.
5. **Apiserver image.** Do we use `ghcr.io/kplane-dev/kplane-apiserver:<latest>` (private until repo goes public) or upstream `registry.k8s.io/kube-apiserver`? The kine→postgres value applies to either. We'll wire both behind a `--apiserver=kplane|upstream` flag and default to kplane.

---

## References

- kplane apiserver storage decorator: `apiserver/pkg/multicluster/storage.go`
- Key layout: `storage/keylayout.go` in `github.com/kplane-dev/storage`
- Sharding (storage-agnostic): `apiserver/pkg/multicluster/sharding/`
- Kine project: <https://github.com/k3s-io/kine> (v0.15.0 released 2026-05-07)
- Kine flow docs: <https://github.com/k3s-io/kine/blob/master/docs/flow.md>
- Upstream apiserver storage interface: <https://github.com/kubernetes/apiserver/tree/master/pkg/storage>
