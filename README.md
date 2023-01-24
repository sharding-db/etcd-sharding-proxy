# Project status

This project is in alpha stage. It is not ready for production use.

For now only KV interface is supported. And it's not fully tested.

# General Architecture
The `Etcd Sharding Proxy` Serves to clients as an etcd endpoint. It proxies requests to the correct `shard` etcd cluster based on the key.
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

# Compatibility
`Revision`, `MemberId`, `ClusterId` of each shard is used. Hence:
- Field `revision` in `Range` / `RangeDelete` requests across different shards will not work.
- `Txn` cannot be executed across multiple shards. NOTE: The proxy will not do check for this. If you use `Txn` across multiple shards, the result is undefined.
- `Compact` is not supported

# Quick Start with Docker
Start 3 etcd and the proxy:
```bash
# start backend etcd
docker run --name etcd-0 -d --rm -p 12379:2379 gcr.io/etcd-development/etcd:v3.5.7 etcd --listen-client-urls http://0.0.0.0:2379 -advertise-client-urls=http://0.0.0.0:2379
docker run --name etcd-1 -d --rm -p 22379:2379 gcr.io/etcd-development/etcd:v3.5.7 etcd --listen-client-urls http://0.0.0.0:2379 -advertise-client-urls=http://0.0.0.0:2379
docker run --name etcd-2 -d --rm -p 32379:2379 gcr.io/etcd-development/etcd:v3.5.7 etcd --listen-client-urls http://0.0.0.0:2379 -advertise-client-urls=http://0.0.0.0:2379

# start proxy
go run ./cmd/proxy -config ./examples/config.yaml
```

Run cmds (need etcdctl installed):
```shell
# try the features
etcdctl put a 1
etcdctl put b 2
etcdctl put c 3
etcdctl get "" --from-key
etcdctl del "" --from-key
etcdctl get "" --from-key
```