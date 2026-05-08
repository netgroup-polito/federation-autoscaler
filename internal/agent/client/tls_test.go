/*
Copyright 2026 Politecnico di Torino - NetGroup.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package client

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTLSConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		c       TLSConfig
		wantErr string
	}{
		{"missing cert", TLSConfig{KeyFile: "k", BrokerCAFile: "ca"}, "cert-file"},
		{"missing key", TLSConfig{CertFile: "c", BrokerCAFile: "ca"}, "key-file"},
		{"missing broker ca", TLSConfig{CertFile: "c", KeyFile: "k"}, "broker-ca-file"},
		{"all set", TLSConfig{CertFile: "c", KeyFile: "k", BrokerCAFile: "ca"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestBuildClientTLSConfig_MissingFile(t *testing.T) {
	c := TLSConfig{
		CertFile:     "/nonexistent/cert",
		KeyFile:      "/nonexistent/key",
		BrokerCAFile: "/nonexistent/ca",
	}
	if _, err := c.BuildClientTLSConfig(); err == nil {
		t.Fatal("expected error from missing files")
	}
}

func TestBuildClientTLSConfig_BadPEM(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad-ca.pem")
	if err := os.WriteFile(bad, []byte("not a pem block"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Write a valid client cert+key so the failure is unambiguously the CA file.
	certPath, keyPath := writeTestKeypair(t, dir)
	cfg := TLSConfig{CertFile: certPath, KeyFile: keyPath, BrokerCAFile: bad}
	if _, err := cfg.BuildClientTLSConfig(); err == nil ||
		!strings.Contains(err.Error(), "no PEM certificates") {
		t.Fatalf("expected 'no PEM certificates' error, got %v", err)
	}
}

func TestBuildClientTLSConfig_HappyPath(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeTestKeypair(t, dir)
	caPath := certPath // self-signed: the cert IS its own CA bundle for this test.

	cfg := TLSConfig{
		CertFile:     certPath,
		KeyFile:      keyPath,
		BrokerCAFile: caPath,
		ServerName:   "broker.test",
	}
	tlsCfg, err := cfg.BuildClientTLSConfig()
	if err != nil {
		t.Fatalf("BuildClientTLSConfig: %v", err)
	}
	if tlsCfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("expected MinVersion TLS 1.3, got %x", tlsCfg.MinVersion)
	}
	if len(tlsCfg.Certificates) != 1 {
		t.Errorf("expected 1 client cert, got %d", len(tlsCfg.Certificates))
	}
	if tlsCfg.RootCAs == nil {
		t.Error("expected RootCAs to be populated")
	}
	if tlsCfg.ServerName != "broker.test" {
		t.Errorf("expected ServerName broker.test, got %q", tlsCfg.ServerName)
	}
}

// writeTestKeypair generates a self-signed ECDSA P-256 cert valid for 1
// hour and writes the cert + key to dir as PEM files. Returns the paths.
// The cert is also a valid CA bundle for itself — handy when tests need a
// "trust me" anchor without spinning up a real PKI.
func writeTestKeypair(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "agent-test"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}

	certPath = filepath.Join(dir, "cert.pem")
	cf, err := os.Create(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	if err := cf.Close(); err != nil {
		t.Fatal(err)
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(dir, "key.pem")
	kf, err := os.Create(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatal(err)
	}
	if err := kf.Close(); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
