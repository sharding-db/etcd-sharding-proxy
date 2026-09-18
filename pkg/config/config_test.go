package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		c     Configurations
		valid bool
	}{
		{"empty", Configurations{}, false},
		{"coordinated", Configurations{Coordinator: &Backend{Endpoint: "c"}, Shards: []Shard{{Address: "a", End: "m"}, {Address: "b", Start: "m", TLS: TLS{Enabled: true}}}}, true},
		{"coordinator missing endpoint", Configurations{Coordinator: &Backend{}, Shards: []Shard{{Address: "a"}}}, false},
		{"coordinator without shards", Configurations{Coordinator: &Backend{Endpoint: "c"}}, false},
		{"backend with coordinator", Configurations{Coordinator: &Backend{Endpoint: "c"}, Backend: &Backend{Endpoint: "b"}}, false},
		{"legacy shard TLS rejected", Configurations{Shards: []Shard{{Address: "a", TLS: TLS{Enabled: true}}}}, false},
		{"invalid coordinator TLS", Configurations{Coordinator: &Backend{Endpoint: "c", TLS: TLS{CertFile: "cert"}}, Shards: []Shard{{Address: "a"}}}, false},
		{"invalid data TLS", Configurations{Coordinator: &Backend{Endpoint: "c"}, Shards: []Shard{{Address: "a", TLS: TLS{KeyFile: "key"}}}}, false},
		{"backend", Configurations{Backend: &Backend{Endpoint: "localhost:2379"}}, true},
		{"empty backend", Configurations{Backend: &Backend{}}, false},
		{"mixed", Configurations{Backend: &Backend{Endpoint: "localhost:2379"}, Shards: []Shard{{Address: "a"}}}, false},
		{"single shard", Configurations{Shards: []Shard{{Address: "a"}}}, true},
		{"adjacent", Configurations{Shards: []Shard{{Address: "a", End: "m"}, {Address: "b", Start: "m"}}}, true},
		{"gap", Configurations{Shards: []Shard{{Address: "a", End: "m"}, {Address: "b", Start: "n"}}}, false},
		{"overlap", Configurations{Shards: []Shard{{Address: "a", End: "m"}, {Address: "b", Start: "a"}}}, false},
		{"backwards", Configurations{Shards: []Shard{{Address: "a", End: "m"}, {Address: "b", Start: "m", End: "a"}, {Address: "c", Start: "a"}}}, false},
		{"no address", Configurations{Shards: []Shard{{}}}, false},
		{"no boundary", Configurations{Shards: []Shard{{Address: "a"}, {Address: "b"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("Validate()=%v, valid=%v", err, tc.valid)
			}
		})
	}
}

func TestFileConfigurationIsolation(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.yaml")
	second := filepath.Join(dir, "second.yaml")
	for path, body := range map[string]string{first: "backend:\n  endpoint: localhost:2379\n  tls:\n    enabled: true\n    serverName: etcd.example\n", second: "shards:\n  - address: localhost:2380\n"} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a, err := NewConfigurationsFromFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Backend.TLS.Enabled || a.Backend.TLS.ServerName != "etcd.example" {
		t.Fatalf("unexpected TLS: %+v", a.Backend.TLS)
	}
	b, err := NewConfigurationsFromFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if b.Backend != nil || len(b.Shards) != 1 {
		t.Fatalf("configuration leaked: %+v", b)
	}
	if _, err := NewConfigurationsFromFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("expected missing file error")
	}
}

func TestTLSValidation(t *testing.T) {
	for _, tc := range []struct {
		c               TLS
		frontend, valid bool
	}{
		{TLS{}, true, true}, {TLS{Enabled: true}, false, true}, {TLS{Enabled: true}, true, false},
		{TLS{CertFile: "cert"}, false, false}, {TLS{KeyFile: "key"}, true, false},
		{TLS{CertFile: "cert", KeyFile: "key", ClientCertAuth: true}, true, false},
		{TLS{CertFile: "cert", KeyFile: "key", ClientCertAuth: true, CAFile: "ca"}, true, true},
		{TLS{ClientCertAuth: true}, false, false}, {TLS{CertFile: "cert", KeyFile: "key", ServerName: "name"}, true, false},
	} {
		if err := tc.c.Validate(tc.frontend); (err == nil) != tc.valid {
			t.Errorf("%+v frontend=%v error=%v", tc.c, tc.frontend, err)
		}
	}
}
