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

# Road Map
- [✅] Support KV APIs
- [✅] Support Watch APIs
- [testing] Support Lease APIs
- Support Auth APIs
- Support Maintenance APIs
- Support TLS
- Basic Metrics
- Performance Test & Tuning for large scale cluster

# Compatibility
`Revision`, `MemberId`, `ClusterId` of each shard is used. Hence:
- Field `revision` in `Range` / `RangeDelete` requests across different shards will not work.
- `Txn` cannot be executed across multiple shards. NOTE: The proxy will not do check for this. If you use `Txn` across multiple shards, the result is undefined.
- `Compact` is not supported
- All `Cluster` APIs are not supported

# About Lease
1. A lease is created in all shards, so that any key can be tied to a lease in all shards. If ID not given, proxy should generate the ID.

2. As in `1.` the lease ID should be the same across all shards. So when list lease, the proxy only list the lease in the first shard.

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