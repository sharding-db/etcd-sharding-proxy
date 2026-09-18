# Kubernetes resource sharding

Use one proxy endpoint per **logical etcd cluster**. Point kube-apiserver's default storage at one endpoint, and use `--etcd-servers-overrides` to place selected built-in resources on another endpoint. Each endpoint preserves its own etcd revision, transactions, watches, leases and compaction. Multiple proxy replicas for one endpoint must connect to the same logical etcd cluster.

The legacy `shards` key-range mode is experimental and **not Kubernetes compatible**. Independent etcd revisions cannot safely be merged into one Kubernetes storage endpoint. In particular, a resource prefix must not span independent revision domains, and a keyless Compact request cannot be broadcast with the same revision to different clusters.

## Configure two proxy endpoints

Default resource proxy (`default.yaml`):

```yaml
backend:
  endpoint: etcd-default.internal:2379
  tls:
    caFile: /certs/backend-ca.crt
    certFile: /certs/proxy-backend.crt
    keyFile: /certs/proxy-backend.key
    # serverName: etcd-default.internal  # optional hostname override

tls:
  certFile: /certs/proxy-server.crt
  keyFile: /certs/proxy-server.key
  caFile: /certs/apiserver-client-ca.crt
  clientCertAuth: true
```

Create a second configuration with `backend.endpoint: etcd-events.internal:2379` for the Events proxy. Use a certificate whose SAN matches each advertised proxy hostname. `backend` and `shards` are mutually exclusive. Backend TLS uses normal certificate and hostname verification; `enabled: true` enables system trust roots when no custom CA is specified. Without TLS settings the connection is plaintext, suitable only for isolated local tests.

Run each proxy with its corresponding configuration:

```bash
go build -o proxy ./cmd/proxy
./proxy -config default.yaml -addr 0.0.0.0 -port 2379
# Run the Events proxy separately with events.yaml.
```

Configure kube-apiserver:

```text
--etcd-servers=https://proxy-default.internal:2379
--etcd-servers-overrides=/events#https://proxy-events.internal:2379
--etcd-cafile=/certs/proxy-ca.crt
--etcd-certfile=/certs/apiserver-etcd-client.crt
--etcd-keyfile=/certs/apiserver-etcd-client.key
```

The override syntax is `group/resource#endpoint`; the core API group is empty, hence `/events`. This Kubernetes option applies to compiled-in resources. CRDs and custom resources remain on the default backend. This is a fresh-cluster configuration: changing overrides on an existing cluster does **not** migrate stored objects.

## Forwarded behavior and limits

The `backend` mode forwards all KV, Watch, Lease and Maintenance RPCs exposed by the bundled etcd protobuf API, including Compact, Status and Snapshot. Request fields, revisions, gRPC metadata (including require-leader), response headers/trailers and error status are preserved. Watch and KeepAlive streams preserve backend semantics; client half-close closes the backend send direction and continues receiving its responses.

Backend responses can exceed the default gRPC 4 MiB limit; Range and Watch tests cover 5 MiB responses. Client request size remains limited to gRPC's default 4 MiB, which exceeds etcd's default 1.5 MiB request limit. Custom etcd deployments allowing larger requests need an accompanying proxy limit change. Listener keepalive enforcement allows pings at intervals of at least five seconds, including without active streams.

Auth and Cluster administration APIs are not implemented. Kubernetes's certificate-based storage path does not call them. Frontend client certificates authorize the client-to-proxy TLS connection; the backend sees the proxy's configured client certificate, not the original client's certificate identity. Use a dedicated, appropriately authorized proxy identity if backend etcd authentication is enabled.

This does not introduce distributed transactions or global revisions across endpoints. Backend endpoint discovery/load balancing, quorum operations, backup policy and online shard migration remain deployment responsibilities. The proxy is stateless; do not reroute an existing endpoint to another independent cluster.

## Repeatable verification

The dedicated [Kubernetes Integration workflow](integration-tests.md) runs both these storage checks and common operations on a full kind cluster. It supports manual dispatch and uploads scenario results and diagnostic logs.

Unit and in-memory gRPC tests:

```bash
go test -race ./...
go vet ./...
```

Download an official [controller-tools envtest release](https://github.com/kubernetes-sigs/controller-tools/releases) for your OS/architecture and verify its SHA-512 checksum. Set `KUBEBUILDER_ASSETS` to the extracted directory containing `etcd` and `kube-apiserver`:

```bash
export KUBEBUILDER_ASSETS=/absolute/path/controller-tools/envtest
go test -tags=integration -race ./...
python3 scripts/k8s-smoke.py --assets "$KUBEBUILDER_ASSETS"
```

The Python harness requires Go and OpenSSL. It starts two independent etcd processes, two proxies and a real kube-apiserver on loopback with temporary test certificates. It does not use kubeconfig or an existing cluster. Child processes are stopped on completion; logs, certificates, data and `result.json` remain in the printed artifact directory. Use a fresh `--workdir` for every run.

The Go integration test verifies real etcd Status, CAS, leases, Watch replay/PrevKV/progress/cancellation and compaction errors. The apiserver harness covers CRUD/CAS conflicts, pagination snapshots under concurrent writes, LIST-to-WATCH replay, WatchList/bookmark, CRD/custom-resource CRUD, physical Events/default-backend placement, TTL Watch DELETE, proxy restart and backend restart. Compactor markers are checked per backend; actual Compact error semantics are checked separately in the Go integration test.

These are storage/control-plane acceptance tests, not a complete Kubernetes conformance suite or a multi-node etcd failover/soak test.
