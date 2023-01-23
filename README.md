Initializing runnable first version

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
- Revision of the shard is used. So `revision` in `Range` / `RangeDelete` requests is accross different shards will not work.

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
```
etcdctl put a 1
etcdctl get a
```