#!/usr/bin/env python3
"""Run real kube-apiserver with backend or coordinated mTLS proxy storage.

Requires Go, openssl, and --assets containing kube-apiserver and etcd (envtest).
No existing cluster/context is used. Logs, certificates and data stay in --workdir
(or a new temporary directory) for diagnosis. Only child processes are stopped.
"""
import argparse
import base64
import json
import os
from pathlib import Path
import socket
import ssl
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


def port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class Suite:
    def __init__(self, args):
        self.args = args
        self.work = Path(args.workdir or tempfile.mkdtemp(prefix="etcd-proxy-k8s-"))
        if self.work.exists() and any(self.work.iterdir()):
            raise ValueError("workdir must be empty: %s" % self.work)
        self.work.mkdir(parents=True, exist_ok=True)
        self.children = []
        self.checks = []
        self.context = None

    def check(self, name):
        self.checks.append(name)
        print("PASS:", name, flush=True)

    def start(self, name, argv):
        log = open(self.work / (name + ".log"), "ab", buffering=0)
        proc = subprocess.Popen([str(x) for x in argv], stdout=log, stderr=subprocess.STDOUT)
        log.close()
        self.children.append(proc)
        return proc

    def stop(self, proc):
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)

    def cleanup(self):
        for proc in reversed(self.children):
            self.stop(proc)

    def request(self, url, method="GET", body=None, expected=200, timeout=10):
        raw = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(url, data=raw, method=method,
                                     headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, context=self.context, timeout=timeout) as resp:
                code, data = resp.status, resp.read()
        except urllib.error.HTTPError as err:
            code, data = err.code, err.read()
        accepted = (expected,) if isinstance(expected, int) else expected
        assert code in accepted, (method, url, code, data[:2000])
        try:
            return json.loads(data)
        except ValueError:
            return data.decode()

    def wait(self, fn, description, timeout=60):
        deadline = time.monotonic() + timeout
        last = None
        while time.monotonic() < deadline:
            try:
                value = fn()
                if value:
                    return value
            except (AssertionError, OSError, urllib.error.URLError) as err:
                last = err
            for child in self.children:
                if child.poll() is not None and child.returncode != -15:
                    raise RuntimeError("child exited: %s; inspect %s" % (child.args, self.work))
            time.sleep(0.25)
        raise AssertionError("timed out: %s (%s); logs: %s" % (description, last, self.work))

    def certs(self):
        def openssl(*args):
            subprocess.run(["openssl", *map(str, args)], check=True,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.ca = self.work / "ca.crt"
        self.ca_key = self.work / "ca.key"
        self.cert = self.work / "client-server.crt"
        self.key = self.work / "client-server.key"
        csr = self.work / "client-server.csr"
        ext = self.work / "cert.ext"
        ext.write_text("basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nsubjectAltName=IP:127.0.0.1,DNS:localhost\nextendedKeyUsage=serverAuth,clientAuth\n")
        openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                "-subj", "/CN=proxy-smoke-ca", "-addext", "basicConstraints=critical,CA:TRUE",
                "-addext", "keyUsage=critical,keyCertSign,cRLSign", "-keyout", self.ca_key, "-out", self.ca)
        openssl("req", "-newkey", "rsa:2048", "-nodes", "-subj",
                "/CN=proxy-smoke-admin/O=system:masters", "-keyout", self.key, "-out", csr)
        openssl("x509", "-req", "-in", csr, "-CA", self.ca, "-CAkey", self.ca_key,
                "-CAcreateserial", "-days", "1", "-extfile", ext, "-out", self.cert)
        self.context = ssl.create_default_context(cafile=str(self.ca))
        self.context.load_cert_chain(str(self.cert), str(self.key))

    def etcd_range(self, shard, prefix):
        key = prefix.encode()
        body = {"key": base64.b64encode(key).decode(),
                "range_end": base64.b64encode(key[:-1] + bytes([key[-1] + 1])).decode()}
        return self.request(self.backends[shard] + "/v3/kv/range", "POST", body)

    def assert_blob(self, key, shard):
        records = self.etcd_range(2, "/__etcd_sharding/keys/" + key).get("kvs", [])
        assert len(records) == 1, (key, records)
        reference = json.loads(base64.b64decode(records[0]["value"]))
        assert reference["shard"] == shard, reference
        blobs = self.etcd_range(shard, reference["key"]).get("kvs", [])
        assert len(blobs) == 1 and blobs[0].get("value"), reference
        assert not self.etcd_range(1 - shard, reference["key"]).get("kvs"), reference

    def run(self):
        print("Artifacts:", self.work, flush=True)
        self.certs()
        binary = self.work / "proxy"
        subprocess.run(["go", "build", "-o", str(binary), "./cmd/proxy"], cwd=ROOT, check=True)
        assets = Path(self.args.assets)
        self.backends, proxies = [], []
        self.proxy_commands = []
        self.proxy_processes = []
        self.etcd_commands, self.etcd_processes = [], []
        for index in range(3 if self.args.mode == "coordinated" else 2):
            client_port, peer_port, proxy_port = port(), port(), port()
            url = "https://127.0.0.1:%d" % client_port
            self.backends.append(url)
            etcd_command = [assets / "etcd", "--name=etcd-%d" % index,
                "--data-dir=" + str(self.work / ("etcd-data-%d" % index)),
                "--listen-client-urls=" + url, "--advertise-client-urls=" + url,
                "--listen-peer-urls=http://127.0.0.1:%d" % peer_port,
                "--initial-advertise-peer-urls=http://127.0.0.1:%d" % peer_port,
                "--initial-cluster=etcd-%d=http://127.0.0.1:%d" % (index, peer_port),
                "--cert-file=" + str(self.cert), "--key-file=" + str(self.key),
                "--trusted-ca-file=" + str(self.ca), "--client-cert-auth=true",
                "--watch-progress-notify-interval=1s"]
            self.etcd_commands.append(etcd_command)
            self.etcd_processes.append(self.start("etcd-%d" % index, etcd_command))
            self.wait(lambda: self.request(url + "/health"), "etcd ready")
        for index in range(1 if self.args.mode == "coordinated" else 2):
            proxy_port = port()
            config = self.work / ("proxy-%d.json" % index)
            tls = {"certFile": str(self.cert), "keyFile": str(self.key), "caFile": str(self.ca)}
            configuration = {"tls": dict(tls, clientCertAuth=True)}
            if self.args.mode == "coordinated":
                boundary = "/registry/configmaps/proxy-smoke/cm-c"
                configuration.update(coordinator={"endpoint": urllib.parse.urlsplit(self.backends[2]).netloc, "tls": tls}, shards=[
                    {"address": urllib.parse.urlsplit(self.backends[0]).netloc, "end": boundary, "tls": tls},
                    {"address": urllib.parse.urlsplit(self.backends[1]).netloc, "start": boundary, "tls": tls}])
            else:
                configuration["backend"] = {"endpoint": urllib.parse.urlsplit(self.backends[index]).netloc, "tls": tls}
            config.write_text(json.dumps(configuration))
            command = [binary, "-config", config, "-addr", "127.0.0.1", "-port", str(proxy_port)]
            self.proxy_commands.append(command)
            self.proxy_processes.append(self.start("proxy-%d" % index, command))
            proxies.append("https://127.0.0.1:%d" % proxy_port)
        api_port = port()
        self.api = "https://127.0.0.1:%d" % api_port
        self.start("kube-apiserver", [assets / "kube-apiserver",
            "--bind-address=127.0.0.1", "--advertise-address=127.0.0.1",
            # No service endpoint publication is needed for this loopback-only
            # storage test; newer Kubernetes rejects loopback endpoint addresses.
            "--endpoint-reconciler-type=none",
            "--secure-port=" + str(api_port), "--service-cluster-ip-range=10.254.0.0/24",
            "--authorization-mode=RBAC", "--anonymous-auth=false",
            "--client-ca-file=" + str(self.ca), "--tls-cert-file=" + str(self.cert),
            "--tls-private-key-file=" + str(self.key),
            "--service-account-signing-key-file=" + str(self.key),
            "--service-account-key-file=" + str(self.key),
            "--service-account-issuer=https://proxy-smoke.local",
            "--etcd-servers=" + proxies[0],
            *(["--etcd-servers-overrides=/events#" + proxies[1]] if self.args.mode == "backend" else []),
            "--etcd-cafile=" + str(self.ca), "--etcd-certfile=" + str(self.cert),
            "--etcd-keyfile=" + str(self.key), "--event-ttl=3s",
            "--etcd-compaction-interval=2s", "--disable-admission-plugins=ServiceAccount",
            "--profiling=false"])
        self.wait(lambda: self.request(self.api + "/readyz") == "ok", "apiserver ready", 90)
        self.check("real kube-apiserver ready through %s mTLS storage" % self.args.mode)
        self.request(self.api + "/api/v1/namespaces", "POST",
                          {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "proxy-smoke"}}, 201)
        path = self.api + "/api/v1/namespaces/proxy-smoke/configmaps"
        def cm(name, value="original"):
            return {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": name}, "data": {"value": value}}
        first = self.request(path, "POST", cm("cm-a"), 201)
        stale = json.loads(json.dumps(first))
        first["data"]["value"] = "updated"
        updated = self.request(path + "/cm-a", "PUT", first)
        self.request(path + "/cm-a", "PUT", stale, 409)
        assert self.request(path + "/cm-a")["data"]["value"] == "updated"
        self.check("create, get, update and stale resourceVersion CAS conflict")
        for name in ("cm-b", "cm-c", "cm-d"):
            self.request(path, "POST", cm(name), 201)
        page = self.request(path + "?limit=2")
        snapshot_rv = page["metadata"]["resourceVersion"]
        items = page["items"]
        assert len(items) == 2 and page["metadata"]["continue"]
        self.request(path, "POST", cm("cm-new"), 201)
        while page["metadata"].get("continue"):
            query = urllib.parse.urlencode({"limit": 2, "continue": page["metadata"]["continue"]})
            page = self.request(path + "?" + query)
            assert page["metadata"]["resourceVersion"] == snapshot_rv
            items.extend(page["items"])
        assert [x["metadata"]["name"] for x in items] == ["cm-a", "cm-b", "cm-c", "cm-d"]
        self.check("limit/continue preserves snapshot during concurrent writes")
        rv = self.request(path)["metadata"]["resourceVersion"]
        self.request(path, "POST", cm("watched"), 201)
        # Request after the write proves historical replay from list RV, not just live delivery.
        watch_url = path + "?" + urllib.parse.urlencode({"watch": "true", "resourceVersion": rv,
                                                        "allowWatchBookmarks": "true", "timeoutSeconds": 20})
        def read_watch():
            with urllib.request.urlopen(watch_url, context=self.context, timeout=25) as response:
                for line in response:
                    event = json.loads(line)
                    if event["type"] == "ADDED" and event["object"]["metadata"]["name"] == "watched":
                        return event
            raise AssertionError("watch replay missing object")
        event = read_watch()
        assert int(event["object"]["metadata"]["resourceVersion"]) > int(rv)
        self.check("LIST -> WATCH revision replay")
        # WatchList drives initial events + bookmark/progress behavior.
        watch_list_url = path + "?" + urllib.parse.urlencode({"watch": "true", "sendInitialEvents": "true",
            "resourceVersionMatch": "NotOlderThan", "allowWatchBookmarks": "true", "timeoutSeconds": 20})
        initial = set()
        bookmarked = False
        with urllib.request.urlopen(watch_list_url, context=self.context, timeout=25) as response:
            for line in response:
                event = json.loads(line)
                if event["type"] == "ADDED":
                    initial.add(event["object"]["metadata"]["name"])
                if event["type"] == "BOOKMARK":
                    bookmarked = True
                    break
        assert bookmarked and "watched" in initial
        self.check("WatchList initial events and bookmark")
        # CRDs intentionally use default storage; overrides cover built-in resources only.
        crds = self.api + "/apis/apiextensions.k8s.io/v1/customresourcedefinitions"
        self.request(crds, "POST", {"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
            "metadata": {"name": "widgets.proxy.test"}, "spec": {"group": "proxy.test", "scope": "Namespaced",
            "names": {"plural": "widgets", "singular": "widget", "kind": "Widget"},
            "versions": [{"name": "v1", "served": True, "storage": True,
                          "schema": {"openAPIV3Schema": {"type": "object", "properties": {
                              "spec": {"type": "object", "x-kubernetes-preserve-unknown-fields": True}}}}}]}}, 201)
        self.wait(lambda: any(x["type"] == "Established" and x["status"] == "True"
                             for x in (self.request(crds + "/widgets.proxy.test")["status"].get("conditions") or [])), "CRD established")
        widgets = self.api + "/apis/proxy.test/v1/namespaces/proxy-smoke/widgets"
        widget = self.request(widgets, "POST", {"apiVersion": "proxy.test/v1", "kind": "Widget",
                                               "metadata": {"name": "one"}, "spec": {"value": 1}}, 201)
        widget["spec"]["value"] = 2
        self.request(widgets + "/one", "PUT", widget)
        assert self.request(widgets + "/one")["spec"]["value"] == 2
        self.request(widgets + "/one", "DELETE")
        self.request(widgets + "/one", expected=404)
        self.check("CRD and custom resource CRUD")
        events = self.api + "/api/v1/namespaces/proxy-smoke/events"
        event_record = self.request(events, "POST", {"apiVersion": "v1", "kind": "Event", "metadata": {"name": "ttl-event"},
            "involvedObject": {"apiVersion": "v1", "kind": "ConfigMap", "name": "cm-a",
                               "namespace": "proxy-smoke", "uid": updated["metadata"]["uid"]}, "reason": "SmokeTest", "message": "TTL expiry", "type": "Normal"}, 201)
        if self.args.mode == "coordinated":
            for name, shard in (("cm-a", 0), ("cm-d", 1)):
                self.assert_blob("/registry/configmaps/proxy-smoke/" + name, shard)
            self.check("same ConfigMap collection physically split across data shards via one endpoint")
        else:
            assert self.etcd_range(0, "/registry/configmaps/proxy-smoke/").get("kvs")
            assert not self.etcd_range(1, "/registry/configmaps/proxy-smoke/").get("kvs")
            assert self.etcd_range(1, "/registry/events/proxy-smoke/").get("kvs")
            assert not self.etcd_range(0, "/registry/events/proxy-smoke/").get("kvs")
            self.check("physical resource sharding: ConfigMaps default, Events override")
        event_watch_url = events + "?" + urllib.parse.urlencode({"watch": "true",
            "resourceVersion": event_record["metadata"]["resourceVersion"], "timeoutSeconds": 30})
        deleted = False
        with urllib.request.urlopen(event_watch_url, context=self.context, timeout=35) as response:
            for line in response:
                event = json.loads(line)
                if event["type"] == "DELETED" and event["object"]["metadata"]["name"] == "ttl-event":
                    deleted = True
                    break
        assert deleted, "Event TTL did not emit watch DELETE"
        self.request(events + "/ttl-event", expected=404)
        self.check("Event TTL expiry emits watch DELETE")
        if self.args.mode == "coordinated":
            self.wait(lambda: self.etcd_range(2, "/__etcd_sharding/keys/compact_rev_key").get("kvs"), "global compactor marker", 20)
            self.check("global coordinator revision domain owns compactor marker")
        else:
            self.wait(lambda: all(self.etcd_range(i, "compact_rev_key").get("kvs") for i in range(2)), "per-backend compactor markers", 20)
            self.check("both revision domains have independent compactor markers")
        self.stop(self.proxy_processes[0])
        self.proxy_processes[0] = self.start("proxy-0-restart", self.proxy_commands[0])
        self.wait(lambda: self.request(path + "/cm-a")["data"]["value"] == "updated", "proxy restart recovery", 30)
        self.request(path, "POST", cm("after-restart"), 201)
        self.check("proxy restart reconnects without losing persisted objects")
        self.stop(self.etcd_processes[0])
        self.etcd_processes[0] = self.start("etcd-0-restart", self.etcd_commands[0])
        self.wait(lambda: self.request(self.backends[0] + "/health"), "backend restart ready", 30)
        # Writes prove storage reconnection; cached GET alone is insufficient.
        def create_after_restart():
            # A timed-out POST may already have committed. Verify that object
            # before accepting a retry conflict as successful recovery.
            self.request(path, "POST", cm("after-backend-restart"), (201, 409))
            return self.request(path + "/after-backend-restart")["data"]["value"] == "original"
        self.wait(create_after_restart, "backend write recovery", 30)
        if self.args.mode == "coordinated":
            self.assert_blob("/registry/configmaps/proxy-smoke/after-backend-restart", 0)
            self.stop(self.etcd_processes[2])
            self.etcd_processes[2] = self.start("coordinator-restart", self.etcd_commands[2])
            self.wait(lambda: self.request(self.backends[2] + "/health"), "coordinator ready", 30)
            self.wait(lambda: self.request(path, "POST", cm("coordinator-recovered"), (201, 409)), "coordinator write recovery", 30)
            assert self.request(path + "/coordinator-recovered")["data"]["value"] == "original"
            self.assert_blob("/registry/configmaps/proxy-smoke/coordinator-recovered", 1)
            self.check("coordinator restart recovers durable references and global revision")
        else:
            assert self.etcd_range(0, "/registry/configmaps/proxy-smoke/after-backend-restart").get("kvs")
        self.check("backend restart reconnects and accepts durable writes")
        self.request(path + "/cm-a", "DELETE")
        self.request(path + "/cm-a", expected=404)
        self.check("delete")
        (self.work / "result.json").write_text(json.dumps({"passed": self.checks, "api": self.api,
            "backends": self.backends, "proxy_endpoints": proxies}, indent=2))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--assets", default=os.environ.get("KUBEBUILDER_ASSETS"), required=not os.environ.get("KUBEBUILDER_ASSETS"))
    parser.add_argument("--workdir")
    parser.add_argument("--mode", choices=("backend", "coordinated"), default="backend")
    args = parser.parse_args()
    suite = Suite(args)
    try:
        suite.run()
        print("All %d checks passed; artifacts retained at %s" % (len(suite.checks), suite.work), flush=True)
    finally:
        suite.cleanup()


if __name__ == "__main__":
    main()
