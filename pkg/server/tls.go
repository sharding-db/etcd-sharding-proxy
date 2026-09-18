package server

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/sharding-db/etcd-sharding-proxy/pkg/config"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// TransportCredentials builds verified TLS for either the listener or backend.
// An empty backend TLS configuration explicitly selects plaintext transport.
func TransportCredentials(c config.TLS, frontend bool) (credentials.TransportCredentials, error) {
	if err := c.Validate(frontend); err != nil {
		return nil, err
	}
	if !c.IsEnabled() {
		return insecure.NewCredentials(), nil
	}
	t := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ServerName}
	if c.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load TLS certificate: %w", err)
		}
		t.Certificates = []tls.Certificate{cert}
	}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read TLS CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("TLS CA file contains no certificates")
		}
		if frontend {
			t.ClientCAs = pool
		} else {
			t.RootCAs = pool
		}
	}
	if frontend && c.ClientCertAuth {
		t.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return credentials.NewTLS(t), nil
}
