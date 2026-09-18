# Kubernetes integration workflow

[Run Kubernetes Integration](https://github.com/sharding-db/etcd-sharding-proxy/actions/workflows/integration.yml) from the Actions tab. Select **Run workflow** and choose `all`, `storage` or `kind`. The workflow also runs automatically for pull requests and main-branch changes affecting Go code, scripts, example configurations or workflows.

```bash
gh workflow run integration.yml --repo sharding-db/etcd-sharding-proxy -f suite=all
```

## Suites

| Suite | Environment | Kubernetes versions | Purpose |
| --- | --- | --- | --- |
| `storage` | Real kube-apiserver, two etcd processes, two mTLS proxies | 1.35.0, 1.37.0 | Revision, Watch, transaction, lease, compaction and reconnect semantics |
| `kind` | Full kind cluster with kubelet, scheduler and controllers; two external etcd containers and two proxies | 1.35.8, 1.37.0 | Common operations including actual Pod readiness and Service traffic |

The kind job uses kind 0.33.0 and digest-pinned node images. Downloaded kind, kubectl and envtest binaries are checksum-verified. Each job has its own isolated environment; the test does not use an existing Kubernetes context or cluster.

## Common operation scenarios

The kind suite checks all of the following:

1. Start the real cluster with its default etcd endpoint pointing at the default proxy and `/events` overridden to the Events proxy. Probe gRPC health and Status through both proxies before bootstrap.
2. Create a Namespace, ConfigMap and Secret; update configuration and secret data.
3. Apply a two-replica Deployment and a Service. Wait for Pod readiness, verify mounted ConfigMap/Secret contents and send HTTP traffic using the Service's cluster DNS name.
4. Scale the Deployment up to three replicas and down to one. Change the Pod template and verify a completed rolling update and the new environment variable inside the Pod.
5. Run a Job, wait for completion and verify its output.
6. Create a ServiceAccount, Role and RoleBinding. Verify that impersonated ConfigMap reads succeed and Secret reads are forbidden.
7. Create an Event and query each etcd backend directly to prove ConfigMaps and Events live on their intended backends and are absent from the other one.
8. Delete an object protected by a finalizer, verify it remains terminating, remove the finalizer and wait for deletion.
9. Delete the Namespace, wait for controller cleanup and verify its ConfigMap and Event storage keys are gone.

The storage suite additionally checks pagination snapshots under concurrent writes, CAS conflicts, Watch replay and progress, WatchList bookmarks, CRD CRUD, Event TTL Watch DELETE, compaction errors and proxy/backend restart recovery. See [storage verification](kubernetes.md#repeatable-verification).

## Results and failure diagnosis

Each matrix job reports independently. The kind job uploads only the `artifacts/` subdirectory:

- `result.json`: overall status, passed scenarios, failing scenario/error and elapsed time.
- `commands.log`: command output from bootstrap, checks and cleanup.
- Proxy and etcd logs, plus kubelet/control-plane logs when available.

The private kubeconfig stays outside uploaded artifacts. Test Secret values are synthetic; no existing cluster credentials are used. Failures retain diagnostics before cleanup. Each command and workflow job has a timeout. The harness deletes only the kind cluster it created and stops only its recorded containers; test network/image remnants are left to the ephemeral runner teardown.

## Run locally

Prerequisites: Docker running, Go, Python 3, kind 0.33.0 and kubectl matching the target Kubernetes minor version.

```bash
python3 scripts/kind-integration.py \
  --node-image kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5 \
  --workdir /absolute/path/to/new-empty-directory
```

The cluster name is unique by default. `--cluster-name` accepts an explicit name but refuses to reuse an existing cluster. Add `--keep` to retain this test's resources for debugging. Use a fresh work directory for each run. Upload or share only its `artifacts/` directory, not the kubeconfig beside it.

## Scope

The full-cluster test uses one control-plane node and plaintext etcd connections on a dedicated Docker network. The separate storage suite exercises mTLS. The isolated network uses kind's experimental custom-network setting and is tested with the pinned kind version.

kind skips kubeadm preflight checks by design. The harness verifies backend readiness through gRPC, but does not test kubeadm's HTTP `/version` preflight against the gRPC-only proxy. The suites are integration acceptance tests, not multi-control-plane HA, online data migration or the complete Kubernetes conformance suite.
