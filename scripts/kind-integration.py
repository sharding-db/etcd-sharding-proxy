#!/usr/bin/env python3
"""Run common operations on an isolated real kind cluster through backend or coordinated proxy storage.
Requires Docker, kind, kubectl and Go. Upload only WORKDIR/artifacts; its parent
contains a private kubeconfig. Only resources created by this run are stopped.
"""
import argparse
import base64
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
ETCD_IMAGE = 'gcr.io/etcd-development/etcd:v3.6.6'
WORKLOAD_IMAGE = 'busybox:1.37.0'


class Suite:
    def __init__(self, args):
        self.args = args
        self.work = Path(args.workdir or tempfile.mkdtemp(prefix='proxy-kind-')).resolve()
        self.work.mkdir(parents=True, exist_ok=True)
        if any(self.work.iterdir()):
            raise ValueError('workdir must be empty: ' + str(self.work))
        self.artifacts = self.work / 'artifacts'
        self.artifacts.mkdir()
        self.token = uuid.uuid4().hex
        self.name = args.cluster_name or 'proxy-' + self.token[:10]
        if not re.fullmatch(r'[a-z0-9][a-z0-9-]{0,39}', self.name):
            raise ValueError('invalid cluster name')
        self.network = self.name + '-' + self.token[:8]
        self.kubeconfig = self.work / 'kubeconfig'
        self.env = dict(os.environ, KUBECONFIG=str(self.kubeconfig), KIND_EXPERIMENTAL_DOCKER_NETWORK=self.network)
        self.containers = {}
        self.cluster_started = False
        self.checks = []
        self.active = 'setup'
        self.start = time.monotonic()

    def cmd(self, argv, timeout=60, data=None, check=True, env=None):
        argv = list(map(str, argv))
        print('+', ' '.join(argv), flush=True)
        result = subprocess.run(argv, input=data, text=True, capture_output=True,
                                timeout=timeout, env=env or self.env, cwd=ROOT)
        with (self.artifacts / 'commands.log').open('a') as log:
            log.write('$ ' + ' '.join(argv) + '\n' + result.stdout + result.stderr)
        if check and result.returncode:
            raise RuntimeError('command failed: %s\n%s' % (' '.join(argv), result.stderr[-4000:]))
        return result

    def k(self, *args, timeout=90, data=None, check=True):
        return self.cmd(['kubectl', '--kubeconfig', self.kubeconfig, '--request-timeout=%ds' % max(1, timeout - 5), *args], timeout, data, check)

    def apply(self, *objects):
        self.k('apply', '-f', '-', data=json.dumps({'apiVersion': 'v1', 'kind': 'List', 'items': objects}))

    def get(self, resource, name):
        return json.loads(self.k('get', resource, name, '-n', 'integration', '-o', 'json').stdout)

    def passed(self):
        self.checks.append({'name': self.active, 'status': 'passed'})
        print('PASS:', self.active, flush=True)

    def wait(self, fn, timeout=90):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            if fn():
                return
            time.sleep(2)
        raise RuntimeError('timed out: ' + self.active)

    def container(self, name, image, *args):
        cid = self.cmd(['docker', 'run', '--detach', '--rm', '--network', self.network,
                        '--label', 'proxy-integration-owner=' + self.token, '--name', self.network + '-' + name,
                        image, *args], timeout=180).stdout.strip()
        self.containers[name] = cid
        return self.cmd(['docker', 'inspect', '--format',
                         '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}', cid]).stdout.strip()

    def keys(self, shard, prefix):
        return self.cmd(['docker', 'exec', self.containers['etcd-' + shard], '/usr/local/bin/etcdctl',
                         '--endpoints=http://127.0.0.1:2379', 'get', prefix, '--prefix', '--keys-only']).stdout.split()

    def etcd_value(self, shard, key):
        result = self.cmd(['docker', 'exec', self.containers['etcd-' + shard], '/usr/local/bin/etcdctl',
                           '--endpoints=http://127.0.0.1:2379', 'get', key, '--write-out=json'])
        values = json.loads(result.stdout).get('kvs', [])
        assert len(values) == 1, (shard, key, values)
        return base64.b64decode(values[0]['value'])

    def assert_blob_placement(self, key, shard, shard_index):
        reference = json.loads(self.etcd_value('coordinator', '/__etcd_sharding/keys/' + key))
        assert reference['shard'] == shard_index, reference
        assert reference['key'].startswith('/__etcd_sharding/blobs/'), reference
        value = self.etcd_value(shard, reference['key'])
        assert value, (shard, key, 'empty Kubernetes object blob')
        other = 'right' if shard == 'left' else 'left'
        assert not self.keys(other, reference['key']), (other, reference)
        assert not self.keys('coordinator', reference['key']), reference

    def setup(self):
        if self.name in self.cmd(['kind', 'get', 'clusters']).stdout.split():
            raise ValueError('refusing to reuse existing kind cluster ' + self.name)
        self.cmd(['docker', 'network', 'create', '--label', 'proxy-integration-owner=' + self.token, self.network])
        build = self.work / 'build'
        build.mkdir()
        arch = {'x86_64': 'amd64', 'amd64': 'amd64', 'aarch64': 'arm64', 'arm64': 'arm64'}[platform.machine()]
        self.cmd(['go', 'build', '-o', build / 'proxy', './cmd/proxy'], timeout=180,
                 env=dict(self.env, CGO_ENABLED='0', GOOS='linux', GOARCH=arch))
        coordinated = self.args.mode == 'coordinated'
        shard_names = ('coordinator', 'left', 'right') if coordinated else ('default', 'events')
        etcd_endpoints = {}
        for shard in shard_names:
            ip = self.container('etcd-' + shard, ETCD_IMAGE, '/usr/local/bin/etcd', '--name=single',
                '--data-dir=/tmp/etcd-data', '--listen-client-urls=http://0.0.0.0:2379',
                '--advertise-client-urls=http://127.0.0.1:2379', '--listen-peer-urls=http://127.0.0.1:2380',
                '--initial-advertise-peer-urls=http://127.0.0.1:2380', '--initial-cluster=single=http://127.0.0.1:2380',
                '--watch-progress-notify-interval=1s')
            self.wait(lambda: self.cmd(['docker', 'exec', self.containers['etcd-' + shard],
                       '/usr/local/bin/etcdctl', 'endpoint', 'health'], check=False).returncode == 0)
            etcd_endpoints[shard] = ip + ':2379'
            if not coordinated:
                (build / (shard + '.json')).write_text(json.dumps({'backend': {'endpoint': ip + ':2379'}}))
        if coordinated:
            boundary = '/registry/pods/integration/m'
            (build / 'coordinated.json').write_text(json.dumps({
                'coordinator': {'endpoint': etcd_endpoints['coordinator']},
                'shards': [{'address': etcd_endpoints['left'], 'end': boundary},
                           {'address': etcd_endpoints['right'], 'start': boundary}]}))
        (build / 'Dockerfile').write_text('FROM scratch\nCOPY proxy /proxy\nCOPY *.json /\nENTRYPOINT ["/proxy"]\n')
        image = 'proxy-integration:' + self.token
        self.cmd(['docker', 'build', '-t', image, build], timeout=180)
        endpoints = {}
        for shard in (('coordinated',) if coordinated else ('default', 'events')):
            health_shard = 'coordinator' if coordinated else shard
            ip = self.container('proxy-' + shard, image, '-config', '/' + shard + '.json', '-addr', '0.0.0.0', '-port', '2379')
            endpoints[shard] = 'http://' + ip + ':2379'
            # kind skips kubeadm preflight; verify actual proxy gRPC health/status.
            self.wait(lambda: self.cmd(['docker', 'exec', self.containers['etcd-' + health_shard],
                '/usr/local/bin/etcdctl', '--endpoints=' + endpoints[shard], 'endpoint', 'health'], check=False).returncode == 0)
            self.cmd(['docker', 'exec', self.containers['etcd-' + health_shard], '/usr/local/bin/etcdctl',
                '--endpoints=' + endpoints[shard], 'endpoint', 'status'])
        config = self.work / 'kind.yaml'
        # No apiVersion: kind converts this mapping for kubeadm v1beta3/v1beta4.
        kind_config = '''kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
kubeadmConfigPatches:
- |
  kind: ClusterConfiguration
  etcd:
    external:
      endpoints:
      - %s
''' % endpoints['coordinated' if coordinated else 'default']
        if not coordinated:
            kind_config += '''  apiServer:
    extraArgs:
      etcd-servers-overrides: /events#%s
''' % endpoints['events']
        config.write_text(kind_config)
        self.cluster_started = True
        self.cmd(['kind', 'create', 'cluster', '--name', self.name, '--image', self.args.node_image,
                  '--config', config, '--kubeconfig', self.kubeconfig, '--wait', '180s', '--retain'], timeout=360)
        self.k('wait', '--for=condition=Ready', 'nodes', '--all', '--timeout=120s', timeout=150)
        self.cmd(['docker', 'pull', WORKLOAD_IMAGE], timeout=180)
        self.cmd(['kind', 'load', 'docker-image', WORKLOAD_IMAGE, '--name', self.name], timeout=180)
        self.active = 'real kubelet, scheduler and controllers ready through sharded proxy storage'
        self.passed()

    def scenarios(self):
        ns = 'integration'
        def obj(kind, name, **fields):
            return dict(apiVersion='v1', kind=kind, metadata={'name': name, 'namespace': ns}, **fields)
        self.active = 'Namespace, ConfigMap and Secret CRUD'
        self.apply({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': ns}},
                   obj('ConfigMap', 'settings', data={'value': 'first'}), obj('Secret', 'credentials', stringData={'value': 'test-only'}))
        self.k('patch', 'configmap', 'settings', '-n', ns, '--type=merge', '-p', '{"data":{"value":"second"}}')
        assert self.get('configmap', 'settings')['data']['value'] == 'second'
        assert self.k('get', 'secret', 'credentials', '-n', ns, '-o', 'jsonpath={.metadata.name}').stdout == 'credentials'
        self.k('patch', 'secret', 'credentials', '-n', ns, '--type=merge', '-p', '{"stringData":{"value":"test-updated"}}')
        self.passed()
        self.active = 'Deployment readiness, Service DNS/HTTP, ConfigMap and Secret mounts'
        deployment = {'apiVersion': 'apps/v1', 'kind': 'Deployment', 'metadata': {'name': 'web', 'namespace': ns},
            'spec': {'replicas': 2, 'selector': {'matchLabels': {'app': 'web'}}, 'template': {
                'metadata': {'labels': {'app': 'web'}}, 'spec': {'containers': [{'name': 'web', 'image': WORKLOAD_IMAGE,
                    'command': ['sh', '-ec', "mkdir -p /www; printf 'proxy-integration-ok' > /www/index.html; exec httpd -f -p 8080 -h /www"],
                    'ports': [{'containerPort': 8080}], 'readinessProbe': {'httpGet': {'path': '/', 'port': 8080}},
                    'volumeMounts': [{'name': 'settings', 'mountPath': '/settings'}, {'name': 'secret', 'mountPath': '/credentials'}]}],
                    'volumes': [{'name': 'settings', 'configMap': {'name': 'settings'}}, {'name': 'secret', 'secret': {'secretName': 'credentials'}}]}}}}
        self.apply(deployment, obj('Service', 'web', spec={'selector': {'app': 'web'}, 'ports': [{'port': 80, 'targetPort': 8080}]}))
        self.rollout()
        self.wait(lambda: self.k('exec', '-n', ns, 'deployment/web', '--', 'sh', '-ec',
             'test "$(cat /settings/value)" = second; test "$(cat /credentials/value)" = test-updated; test "$(wget -T 10 -qO- http://web.integration.svc.cluster.local)" = proxy-integration-ok', check=False).returncode == 0)
        self.passed()
        self.active = 'Deployment scale up/down and rolling template update'
        for replicas in (3, 1):
            self.k('scale', 'deployment/web', '-n', ns, '--replicas=' + str(replicas))
            self.rollout()
            self.wait(lambda: self.get('deployment', 'web').get('status', {}).get('readyReplicas') == replicas)
        before = self.get('deployment', 'web')['metadata']['generation']
        self.k('set', 'env', 'deployment/web', '-n', ns, 'ROLLOUT_MARKER=updated')
        self.rollout()
        assert self.get('deployment', 'web')['metadata']['generation'] > before
        assert self.k('exec', '-n', ns, 'deployment/web', '--', 'printenv', 'ROLLOUT_MARKER').stdout.strip() == 'updated'
        self.passed()
        self.active = 'Job scheduling and completion'
        self.apply({'apiVersion': 'batch/v1', 'kind': 'Job', 'metadata': {'name': 'once', 'namespace': ns},
            'spec': {'backoffLimit': 0, 'template': {'spec': {'restartPolicy': 'Never', 'containers': [
                {'name': 'once', 'image': WORKLOAD_IMAGE, 'command': ['sh', '-ec', 'echo job-complete']}]}}}})
        self.k('wait', '--for=condition=complete', 'job/once', '-n', ns, '--timeout=120s', timeout=150)
        assert 'job-complete' in self.k('logs', 'job/once', '-n', ns).stdout
        self.passed()
        self.active = 'RBAC allows ConfigMap read and denies Secret read'
        self.apply(obj('ServiceAccount', 'reader'),
            {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'Role', 'metadata': {'name': 'reader', 'namespace': ns},
             'rules': [{'apiGroups': [''], 'resources': ['configmaps'], 'verbs': ['get', 'list']}]},
            {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'RoleBinding', 'metadata': {'name': 'reader', 'namespace': ns},
             'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': 'reader'},
             'subjects': [{'kind': 'ServiceAccount', 'name': 'reader', 'namespace': ns}]})
        identity = '--as=system:serviceaccount:integration:reader'
        self.wait(lambda: self.k('get', 'configmap', 'settings', '-n', ns, identity, check=False).returncode == 0)
        denied = self.k('get', 'secret', 'credentials', '-n', ns, identity, check=False)
        assert denied.returncode != 0 and 'Forbidden' in denied.stderr
        self.passed()
        if self.args.mode == 'coordinated':
            self.active = 'same namespace Pods across data shards with single endpoint LIST/WATCH'
            self.apply(obj('Pod', 'alpha', spec={'containers': [{'name': 'alpha', 'image': WORKLOAD_IMAGE,
                'command': ['sh', '-ec', 'sleep 3600']}]}))
            self.k('wait', '--for=condition=Ready', 'pod/alpha', '-n', ns, '--timeout=120s', timeout=150)
            snapshot = json.loads(self.k('get', 'pods', '-n', ns, '--chunk-size=1', '-o', 'json').stdout)
            pods = snapshot['items']
            names = [pod['metadata']['name'] for pod in pods]
            assert 'alpha' in names and any(name.startswith('web-') for name in names), names
            watched_names = {'alpha', next(name for name in names if name.startswith('web-'))}
            for name in sorted(watched_names):
                self.k('annotate', 'pod', name, '-n', ns, 'integration.proxy.test/replay=verified')
            replay = self.k('get', '--raw', '/api/v1/namespaces/integration/pods?watch=true&resourceVersion='
                + snapshot['metadata']['resourceVersion'] + '&timeoutSeconds=5', timeout=20).stdout
            observed = set()
            for line in replay.splitlines():
                event = json.loads(line)
                assert event['type'] != 'ERROR', event
                metadata = event['object']['metadata']
                if metadata.get('annotations', {}).get('integration.proxy.test/replay') == 'verified':
                    observed.add(metadata['name'])
                    assert int(metadata['resourceVersion']) > int(snapshot['metadata']['resourceVersion'])
            assert watched_names <= observed, (watched_names, observed)
            self.assert_blob_placement('/registry/pods/integration/alpha', 'left', 0)
            for name in names:
                if name.startswith('web-'):
                    self.assert_blob_placement('/registry/pods/integration/' + name, 'right', 1)
            self.passed()
        else:
            self.active = 'physical ConfigMap/default and Event/override storage placement'
            self.apply(obj('Event', 'placement', involvedObject={'apiVersion': 'v1', 'kind': 'ConfigMap', 'name': 'settings',
                'namespace': ns, 'uid': self.get('configmap', 'settings')['metadata']['uid']}, reason='Integration', message='placement', type='Normal'))
            for shard, prefix, present in [('default', '/registry/configmaps/integration/settings', True),
                ('events', '/registry/configmaps/integration/settings', False), ('events', '/registry/events/integration/placement', True),
                ('default', '/registry/events/integration/placement', False)]:
                assert bool(self.keys(shard, prefix)) == present, (shard, prefix, present)
            self.passed()
        self.active = 'finalizer blocks deletion until released'
        final = obj('ConfigMap', 'finalized', data={'test': 'finalizer'})
        final['metadata']['finalizers'] = ['integration.proxy.test/hold']
        self.apply(final)
        self.k('delete', 'configmap', 'finalized', '-n', ns, '--wait=false')
        assert self.get('configmap', 'finalized')['metadata'].get('deletionTimestamp')
        self.k('patch', 'configmap', 'finalized', '-n', ns, '--type=merge', '-p', '{"metadata":{"finalizers":[]}}')
        self.k('wait', '--for=delete', 'configmap/finalized', '-n', ns, '--timeout=60s')
        self.passed()
        self.active = 'namespace controller removes workloads and storage'
        self.k('delete', 'namespace', ns, '--wait=true', '--timeout=120s', timeout=150)
        if self.args.mode == 'coordinated':
            metadata_keys = self.keys('coordinator', '/__etcd_sharding/keys//registry/')
            assert not [key for key in metadata_keys if '/integration/' in key or key.endswith('/namespaces/integration')], metadata_keys
            # Immutable data blobs deliberately remain for MVCC history; visible metadata must be gone.
        else:
            assert not self.keys('default', '/registry/configmaps/integration/')
            assert not self.keys('events', '/registry/events/integration/')
        self.passed()

    def rollout(self):
        self.k('rollout', 'status', 'deployment/web', '-n', 'integration', '--timeout=120s', timeout=150)

    def diagnostics(self):
        commands = [(name, ['docker', 'logs', cid]) for name, cid in self.containers.items()]
        if self.cluster_started:
            node = self.name + '-control-plane'
            commands.extend([('kubelet', ['docker', 'exec', node, 'journalctl', '-u', 'kubelet', '--no-pager']),
                ('control-plane', ['docker', 'exec', node, 'sh', '-c', "find /var/log/pods -type f -name '*.log' -exec cat {} +"])])
        for name, command in commands:
            try:
                result = self.cmd(command, check=False)
                (self.artifacts / (name + '.log')).write_text(result.stdout + result.stderr)
            except Exception as error:
                print('diagnostic error:', error, flush=True)

    def cleanup(self):
        if self.args.keep:
            print('Resources retained (--keep):', self.name, self.network)
            return
        errors = []
        if self.cluster_started:
            try:
                self.cmd(['kind', 'delete', 'cluster', '--name', self.name], timeout=120)
            except Exception as error:
                errors.append(str(error))
        for cid in reversed(list(self.containers.values())):
            try:
                self.cmd(['docker', 'stop', '--time=10', cid], timeout=30)
            except Exception as error:
                errors.append(str(error))
        # Network/image retained for runner teardown; no unrelated resources removed.
        if errors:
            raise RuntimeError('; '.join(errors))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--node-image', required=True)
    parser.add_argument('--mode', choices=('backend', 'coordinated'), default='backend')
    parser.add_argument('--workdir')
    parser.add_argument('--cluster-name')
    parser.add_argument('--keep', action='store_true', help='retain owned test resources for debugging')
    args = parser.parse_args()
    suite = Suite(args)
    error = None
    try:
        suite.setup()
        suite.scenarios()
    except Exception as exc:
        error = str(exc)
        print('FAIL:', suite.active, error, flush=True)
    finally:
        suite.diagnostics()
        try:
            suite.cleanup()
        except Exception as exc:
            error = (error + '; ' if error else '') + 'cleanup: ' + str(exc)
        (suite.artifacts / 'result.json').write_text(json.dumps({'status': 'failed' if error else 'passed',
            'node_image': args.node_image, 'mode': args.mode, 'cluster': suite.name, 'checks': suite.checks,
            'failed_scenario': suite.active if error else None, 'error': error,
            'elapsed_seconds': round(time.monotonic() - suite.start, 1)}, indent=2) + '\n')
        print('Public artifacts:', suite.artifacts, flush=True)
    return 1 if error else 0


if __name__ == '__main__':
    raise SystemExit(main())
