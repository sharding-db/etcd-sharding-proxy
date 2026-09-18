# Etcd Sharding Proxy

[![Test](https://github.com/sharding-db/etcd-sharding-proxy/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/sharding-db/etcd-sharding-proxy/actions/workflows/test.yml)
[![Kubernetes Integration](https://github.com/sharding-db/etcd-sharding-proxy/actions/workflows/integration.yml/badge.svg?branch=main)](https://github.com/sharding-db/etcd-sharding-proxy/actions/workflows/integration.yml)

An etcd gRPC proxy exploring transparent key-range sharding for Kubernetes. The experimental coordinated mode splits the same resource collection across data shards behind one endpoint, using a coordinator for global MVCC. Backend mode also supports resource-based placement via kube-apiserver overrides.

## Single endpoint key-range sharding (PoC)

Add `coordinator` alongside `shards` to enable global revisions, consistent
cross-shard LIST/WATCH, CAS, leases and compaction. Data shards store immutable
values; coordinator etcd atomically publishes their references. This preserves
the original single-endpoint goal without modifying kube-apiserver.

See [architecture, configuration and limitations](docs/coordinated-sharding.md)
and [example configuration](examples/coordinated.yaml). **Not production ready:**
there is no blob garbage collection or coordinated backup/restore. Every key's
metadata still lives in the coordinator, so this PoC does not establish linear
write-throughput or key-count scaling. Cross-shard mutations, nested transactions
and VALUE comparisons/sorting are rejected.

## Kubernetes resource sharding

```text
                          kube-apiserver
                                |
              +-----------------+-----------------+
              |                                   |
       default resources                       Events
              |                                   |
     proxy-default:2379                   proxy-events:2379
       backend mode                         backend mode
              |                                   |
     etcd default cluster                  etcd Events cluster
```

Each endpoint preserves its backend's revisions, transactions, watches, leases and compaction. There is no shared global revision or distributed transaction across independent clusters.

Configure kube-apiserver with separate endpoints:

```text
--etcd-servers=https://proxy-default.internal:2379
--etcd-servers-overrides=/events#https://proxy-events.internal:2379
--etcd-cafile=/certs/proxy-ca.crt
--etcd-certfile=/certs/apiserver-etcd-client.crt
--etcd-keyfile=/certs/apiserver-etcd-client.key
```

Overrides apply to built-in resources. CRDs and custom resources remain on the default backend. Changing overrides on an existing cluster does not migrate stored data.

See [Kubernetes configuration and verification](docs/kubernetes.md) for proxy configuration, TLS/mTLS, deployment boundaries and repeatable acceptance tests.

## Modes and capabilities

| | `backend` mode | Legacy `shards` mode |
| --- | --- | --- |
| Routing | One logical etcd cluster per endpoint | Key ranges across independent etcd clusters |
| Kubernetes | Resource sharding via separate endpoints | Not compatible |
| KV and transactions | Forwarded with native backend semantics | Experimental; cross-shard transactions are unsafe |
| Watch | Native IDs, revisions, replay, progress and cancellation | Experimental; no global revision or event ordering |
| Lease | Native backend lease lifecycle | Same lease ID replicated across shards; non-atomic |
| Maintenance | Forwarded, including Compact, Status and Snapshot | Not implemented |
| TLS/mTLS | Listener and backend configured independently | Listener TLS only; backend connections are plaintext |

`backend` is mutually exclusive with `shards` and `coordinator`. `shards` alone retains legacy behavior; adding `coordinator` selects the new PoC described above. Auth and Cluster administration APIs are not implemented.

## Quick start: one backend

Prerequisites: Go, `etcd` and `etcdctl`. CI uses the current stable Go toolchain. The commands below use plaintext loopback connections for local testing; use [TLS/mTLS](docs/kubernetes.md#configure-two-proxy-endpoints) for networked deployments.

```bash
git clone https://github.com/sharding-db/etcd-sharding-proxy.git
cd etcd-sharding-proxy
```

Start a local etcd in one terminal:

```bash
etcd --name default \
  --data-dir ./etcd-0.etcd \
  --listen-client-urls http://127.0.0.1:12379 \
  --advertise-client-urls http://127.0.0.1:12379 \
  --listen-peer-urls http://127.0.0.1:12380 \
  --initial-advertise-peer-urls http://127.0.0.1:12380 \
  --initial-cluster default=http://127.0.0.1:12380
```

Start the proxy in a second terminal using [examples/kubernetes.yaml](examples/kubernetes.yaml):

```bash
go run ./cmd/proxy -config ./examples/kubernetes.yaml -addr 127.0.0.1 -port 2379
```

The configuration selects the local backend:

```yaml
backend:
  endpoint: 127.0.0.1:12379
```

Use an ordinary etcd client against the proxy:

```bash
etcdctl --endpoints=http://127.0.0.1:2379 put hello world
etcdctl --endpoints=http://127.0.0.1:2379 get hello
etcdctl --endpoints=http://127.0.0.1:2379 endpoint status --write-out=table
```

For resource sharding, run another proxy against a separate etcd cluster and configure the corresponding kube-apiserver override. Multiple replicas of the same proxy endpoint must connect to the same logical etcd cluster.

## Validation

CI runs unit/race tests and a dedicated [Kubernetes Integration workflow](.github/workflows/integration.yml). Its storage suite uses real Kubernetes **1.35.0** and **1.37.0** with mTLS and two independent data backends, plus a coordinator in coordinated mode. Both backend and coordinated modes run in the storage suite. Its full kind suite uses **1.35.8** and **1.37.0** in both modes to check Deployment rollout/scaling, Service DNS/HTTP, ConfigMap/Secret mounts, Job completion, RBAC, finalizers and namespace cleanup.

[Run the workflow and inspect scenario results](docs/integration-tests.md); it supports manual dispatch with `all`, `storage` or `kind`.

The Kubernetes harness verifies:

- CRUD and stale resourceVersion/CAS conflicts.
- Stable pagination snapshots during concurrent writes.
- LIST-to-WATCH replay and WatchList initial events/bookmarks.
- CRD and custom-resource CRUD on the default backend.
- Physical separation of ConfigMaps and Events across backends.
- Event TTL expiration and its Watch DELETE event.
- Independent compactor markers and proxy/backend restart recovery.

Separate real-etcd integration tests verify Watch progress, PrevKV, cancellation, lease lifecycle, Status and historical Range/Watch errors after compaction. Regression tests also cover 5 MiB Range and Watch responses.

Run unit tests and static checks:

```bash
go test -race ./...
go vet ./...
```

With verified [envtest binaries](https://github.com/kubernetes-sigs/controller-tools/releases) installed:

```bash
export KUBEBUILDER_ASSETS=/absolute/path/controller-tools/envtest
go test -tags=integration -race ./...
python3 scripts/k8s-smoke.py --assets "$KUBEBUILDER_ASSETS"
```

The harness needs Python 3 and OpenSSL. It creates isolated loopback processes, does not use an existing cluster or kubeconfig, and retains logs and results in its printed artifact directory. These checks cover the Kubernetes storage/control-plane path; they are not a complete conformance or multi-node HA/soak suite.

## Compatibility boundaries

- Backend mode forwards the KV, Watch, Lease and Maintenance RPCs exposed by the bundled etcd protobuf API, preserving request fields, revisions, metadata, response headers/trailers and gRPC errors.
- Client requests retain gRPC's default 4 MiB proxy limit. Backend responses support larger messages. Custom etcd deployments accepting larger requests need a corresponding proxy limit change.
- Frontend mTLS authenticates the connection to the proxy. The backend sees the proxy's configured certificate identity, not the original client's certificate identity.
- Endpoint discovery/load balancing, etcd quorum management, backup policy and online migration remain deployment responsibilities.

### Legacy key-range mode

[examples/config.yaml](examples/config.yaml) retains the original three-shard example. This mode is experimental and must not be used as a Kubernetes storage endpoint.

Each shard has independent revisions and cluster/member IDs. Cross-shard historical snapshots and globally ordered watches are not supported. Transactions are not checked for cross-shard operations; their results are undefined when they span shards. Compact is unavailable.

A lease is granted on every shard with the same ID. KeepAlive renews all shards and returns one response with the minimum reported TTL. TTL queries aggregate keys across shards; listing reads the first shard. Grant/revoke failures can leave partial state, and the proxy does not reconcile it.

## Roadmap

- Coordinated-mode blob GC, backup/restore and batched value reads.
- Benchmarks for coordinator limits and payload capacity scaling.
- Metrics and operational observability.
- Large-scale performance and multi-node failure testing.
- Auth and Cluster administration APIs.
- Explicit migration and recovery workflows for resource placement changes.

## License

[MIT](LICENSE)
