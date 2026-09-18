# Introduction
`Etcd Sharding Proxy` is a lightweight solution to scale etcd cluster by sharding. It is designed to be compatible with etcd client and easy to deploy.

```text
                          │
                          │
                          │
                 ┌────────▼────────┐
                 │  Load Balancer  │
                 └────────┬────────┘
                          │
                          │
                          │
             ┌────────────▼────────────┐
             │   Etcd Sharding Proxy   │
             └────────────┬────────────┘
                          │
                          │
        ┌─────────────────┼─────────────────┐
        │                 │                 │
┌───────▼───────┐ ┌───────▼───────┐ ┌───────▼───────┐
│               │ │               │ │               │
│  Etcd Cluster │ │  Etcd Cluster │ │  Etcd Cluster │
│               │ │               │ │               │
└───────────────┘ └───────────────┘ └───────────────┘
```

The `Etcd Sharding Proxy` Serves to clients as an etcd endpoint. It proxies requests to the correct `shard` etcd cluster based on the key.

# Kubernetes

For Kubernetes, use the `backend` mode with one endpoint per logical etcd cluster and kube-apiserver resource overrides for sharding. This preserves revisions, transactions, Watch, Lease, Compact and Status semantics and supports TLS/mTLS on both connections. See [Kubernetes setup and verification](docs/kubernetes.md).

The original `shards` key-range mode below is experimental and not Kubernetes compatible. It does not provide global revisions or atomic cross-shard transactions.

# Road Map
- [✅] Support KV APIs
- [✅] Support Watch APIs
- [testing] Support Lease APIs
- Support Auth APIs
- [backend mode] Support Maintenance APIs
- [backend mode] Support TLS/mTLS
- Basic Metrics
- Performance Test & Tuning for large scale cluster

# Legacy key-range mode compatibility
`Revision`, `MemberId`, `ClusterId` of each shard is used. Hence:
- Field `revision` in `Range` / `RangeDelete` requests across different shards will not work.
- `Txn` cannot be executed across multiple shards. NOTE: The proxy will not do check for this. If you use `Txn` across multiple shards, the result is undefined.
- `Compact` is not supported
- All `Cluster` APIs are not supported

# About Lease
1. A lease is created in all shards, so that any key can be tied to a lease in all shards. If ID not given, proxy should generate the ID.

2. As in `1.` the lease ID should be the same across all shards. So when list lease, the proxy only list the lease in the first shard.

3. Each keepalive request renews the lease on all shards and returns one response. The response TTL is the minimum reported by any shard; a zero TTL means the lease is missing on at least one shard. Backend stream errors terminate the client stream.

4. Time-to-live queries return the minimum shard TTL and, when requested, keys from every shard.

Lease operations across shards are not atomic. A grant or revoke failure may leave partial state; this proxy does not currently reconcile it. Listing the first shard assumes the lease exists consistently across shards.

# Tests
```bash
go test -race ./...
```
Lease tests cover key aggregation, listing, keepalive responses, backend failures, cancellation, and client half-close using in-memory gRPC transport. They do not replace validation against real multi-shard etcd clusters or Kubernetes.

# Quick Start with Docker
```bash
# Clone the repo
git clone https://github.com/sharding-db/etcd-sharding-proxy.git

# start backend etcd
docker run --name etcd-0 -d --rm -p 12379:2379 gcr.io/etcd-development/etcd:v3.5.7 etcd --listen-client-urls http://0.0.0.0:2379 -advertise-client-urls=http://0.0.0.0:2379
docker run --name etcd-1 -d --rm -p 22379:2379 gcr.io/etcd-development/etcd:v3.5.7 etcd --listen-client-urls http://0.0.0.0:2379 -advertise-client-urls=http://0.0.0.0:2379
docker run --name etcd-2 -d --rm -p 32379:2379 gcr.io/etcd-development/etcd:v3.5.7 etcd --listen-client-urls http://0.0.0.0:2379 -advertise-client-urls=http://0.0.0.0:2379

# start proxy
go run ./cmd/proxy -config ./examples/config.yaml &

# try the features
etcdctl put a 1
etcdctl put j 2
etcdctl put z 3
etcdctl get "" --from-key
```

# Start with local etcd
```bash
mkdir /etcd-log
# start backend etcd
etcd --name etcd-0 --listen-client-urls http://0.0.0.0:12379 --listen-peer-urls http://0.0.0.0:12380 -advertise-client-urls=http://0.0.0.0:12379 1> ./etcd-log/etcd-0.log 2>&1 &
etcd --name etcd-1 --listen-client-urls http://0.0.0.0:22379 --listen-peer-urls http://0.0.0.0:22380 -advertise-client-urls=http://0.0.0.0:22379 1> ./etcd-log/etcd-1.log 2>&1 &
etcd --name etcd-2 --listen-client-urls http://0.0.0.0:32379 --listen-peer-urls http://0.0.0.0:32380 -advertise-client-urls=http://0.0.0.0:32379 1> ./etcd-log/etcd-2.log 2>&1 &

# start proxy
go run ./cmd/proxy -config ./examples/config.yaml &

# try the features
etcdctl put a 1
etcdctl put j 2
etcdctl put z 3
etcdctl get "" --from-key
```