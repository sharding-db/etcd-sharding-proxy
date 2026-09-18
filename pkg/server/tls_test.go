package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sharding-db/etcd-sharding-proxy/pkg/config"
)

func TestTransportCredentials(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	badFile := filepath.Join(dir, "bad.pem")
	for path, data := range map[string][]byte{certFile: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyFile: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), badFile: []byte("invalid")} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name            string
		c               config.TLS
		frontend, valid bool
		protocol        string
	}{
		{"plaintext", config.TLS{}, false, true, "insecure"},
		{"system roots", config.TLS{Enabled: true}, false, true, "tls"},
		{"listener", config.TLS{CertFile: certFile, KeyFile: keyFile, CAFile: certFile, ClientCertAuth: true}, true, true, "tls"},
		{"backend", config.TLS{CertFile: certFile, KeyFile: keyFile, CAFile: certFile, ServerName: "localhost"}, false, true, "tls"},
		{"missing cert", config.TLS{CertFile: "missing", KeyFile: "missing"}, true, false, ""},
		{"missing ca", config.TLS{CAFile: "missing"}, false, false, ""},
		{"bad ca", config.TLS{CAFile: badFile}, false, false, ""},
		{"invalid config", config.TLS{CertFile: certFile}, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creds, err := TransportCredentials(tc.c, tc.frontend)
			if (err == nil) != tc.valid {
				t.Fatalf("credentials error=%v", err)
			}
			if err == nil && creds.Info().SecurityProtocol != tc.protocol {
				t.Fatalf("protocol=%s", creds.Info().SecurityProtocol)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		client config.TLS
		valid  bool
	}{
		{"mutual TLS handshake", config.TLS{CertFile: certFile, KeyFile: keyFile, CAFile: certFile, ServerName: "localhost"}, true},
		{"reject anonymous client", config.TLS{CAFile: certFile, ServerName: "localhost"}, false},
		{"reject wrong server name", config.TLS{CertFile: certFile, KeyFile: keyFile, CAFile: certFile, ServerName: "other.example"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serverCreds, err := TransportCredentials(config.TLS{CertFile: certFile, KeyFile: keyFile, CAFile: certFile, ClientCertAuth: true}, true)
			if err != nil {
				t.Fatal(err)
			}
			clientCreds, err := TransportCredentials(tc.client, false)
			if err != nil {
				t.Fatal(err)
			}
			serverConn, clientConn := net.Pipe()
			defer serverConn.Close()
			defer clientConn.Close()
			_ = serverConn.SetDeadline(time.Now().Add(3 * time.Second))
			_ = clientConn.SetDeadline(time.Now().Add(3 * time.Second))
			done := make(chan error, 1)
			go func() { _, _, err := serverCreds.ServerHandshake(serverConn); done <- err }()
			_, _, clientErr := clientCreds.ClientHandshake(context.Background(), "localhost", clientConn)
			if clientErr != nil {
				_ = clientConn.Close()
			}
			serverErr := <-done
			if tc.valid && (clientErr != nil || serverErr != nil) {
				t.Fatalf("client=%v server=%v", clientErr, serverErr)
			}
			if !tc.valid && clientErr == nil && serverErr == nil {
				t.Fatal("untrusted TLS peer accepted")
			}
		})
	}

}
