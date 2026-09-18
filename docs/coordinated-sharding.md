# Coordinated key-range sharding (PoC)

The original goal is to split one keyspace, including one Kubernetes resource
collection, behind a single endpoint. `coordinator` plus `shards` implements a
bounded proof of concept of that goal. Unlike resource overrides, a LIST of Pods
can span both data shards without changing kube-apiserver.

## Storage protocol

```text
kube-apiserver -> proxy endpoint -> coordinator etcd: keys, MVCC, leases, references
                        |-------> data shard 0: immutable values
                        `-------> data shard 1: immutable values
```

A Put first writes a random immutable blob to the key's data shard. Only after
that write succeeds does the proxy publish its reference in coordinator etcd.
Coordinator metadata uses the original key with an internal prefix. Its native
create/mod revisions and version are the user-visible revisions and version.
Flat transactions publish references atomically using native etcd comparisons.
Reads fetch references at one coordinator snapshot and resolve immutable blobs,
so pagination and historical reads do not mix versions from different shards.
Watch events use the same coordinator history; progress notifications cannot
pass events still being materialized on that stream.

A crash before publication can leave an unreachable blob. A crash or lost reply
after publication leaves a durable, readable value; a client still has ordinary
etcd ambiguity about whether a timed-out mutation committed. Restarting the proxy
requires no in-memory revision reconstruction. Multiple proxy replicas may use
the same coordinator and identical routing configuration.

Leases attach to metadata keys only. Native expiry/revoke generates coordinator
DELETE events; associated blobs remain available for historical reads and
PrevKV. Compact removes coordinator history, not blob data. A routing fingerprint
is initialized atomically and rejects changed boundaries, shard order or endpoint
strings on restart. It does not detect aliases or an endpoint repointed to another
cluster; deployment must keep cluster identities stable.

## Configuration

Start **three dedicated etcd clusters** on the configured endpoints. Do not write
to their internal keys directly. Existing legacy data cannot be reused as this
format; there is no in-place migration.

```yaml
coordinator:
  endpoint: 127.0.0.1:12379
shards:
  - address: 127.0.0.1:22379
    end: /registry/pods/integration/m
  - address: 127.0.0.1:32379
    start: /registry/pods/integration/m
```

Run `go run ./cmd/proxy -config examples/coordinated.yaml -addr 127.0.0.1`.
Point kube-apiserver's `--etcd-servers` at this one proxy endpoint, with **no
`--etcd-servers-overrides`**. Pods `integration/alpha` and `integration/web-*`
then go to different data shards. Boundaries are bytewise and static; the first
start and last end are unbounded. Endpoint values are gRPC `host:port` targets,
not HTTP URLs. Listener `tls`, `coordinator.tls`, and each shard's `tls` accept
the same TLS/mTLS options as backend mode.

## Supported scope and limits

- Global coordinator revision, historical Range/pagination, metadata comparisons
  (version/create/mod/lease), flat same-shard mutating Txn, cross-shard read-only
  Txn, Watch replay/progress/cancellation/PrevKV, leases and compaction.
- Cross-shard mutating Txn and DeleteRange, nested Txn, VALUE comparisons and VALUE
  sorting are rejected. Both Txn branches must meet this scope before staging.
  Lease expiry/revoke can delete keys across shards atomically in the coordinator.
- Both Txn branches pre-stage their values. Failed comparisons and unselected
  branches may leave orphan blobs. An unavailable data shard can fail a Txn even
  when its selected branch would only read.
- **No garbage collection.** All historical values and orphan blobs accumulate
  indefinitely. This mode is not ready for production or bounded-capacity storage.
- The coordinator still stores **every key and every metadata mutation** and
  sequences all commits. Payload capacity is distributed; key-count capacity and
  metadata write throughput do not scale linearly with shard count. Reads resolve
  values sequentially and add network round trips. No throughput improvement is
  claimed by this PoC.
- Status reports the coordinator's status, not aggregate health/capacity. Other
  Maintenance APIs, Auth and Cluster administration are unimplemented. There is
  no coordinated snapshot/restore, online migration or failover orchestration.
- Dedicated clusters, durable etcd storage and stable routing are required.
  A missing blob returns DataLoss; a failed shard read returns an error, not an
  incomplete successful LIST. Reading metadata-only keys may still succeed.

For the original goal of scalable transparent etcd, this demonstrates a viable
consistency path, not the final architecture. Further work requires safe MVCC-aware
blob GC, coordinated backup/restore, batched/parallel reads, resource budgets and
benchmarks. Removing the centralized metadata ceiling would require a distributed
MVCC/transaction/watch protocol or changes to the Kubernetes storage contract.

## Repeatable acceptance

```bash
export KUBEBUILDER_ASSETS=/absolute/path/controller-tools/envtest
go test -tags=integration -race ./...
python3 scripts/k8s-smoke.py --mode coordinated
python3 scripts/kind-integration.py --mode coordinated --node-image <pinned-kind-image>
```

The storage harness runs real kube-apiserver and three etcd processes with mTLS.
It proves same-collection ConfigMap placement, stable paginated snapshots, CAS,
LIST-to-WATCH replay, WatchList, Event TTL, compaction and proxy/data/coordinator
restart recovery. Real-etcd tests inject lost successful blob and commit replies,
shard unavailability, concurrent proxy writes and routing mismatch. The kind
workflow adds real Pods on both shards and Deployment/Service/Job/RBAC/finalizer/
namespace-controller operations. These are scoped integration checks, not the
Kubernetes conformance suite or a multi-member etcd disaster-recovery test.
