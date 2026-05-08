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

package integration

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// certBundle is the in-test PKI: a self-signed CA whose private key
// signs both the broker server cert (with a 127.0.0.1 SAN so the agent
// can dial the listener) and one or more agent client certs (with the
// agent's cluster ID as CN, since the broker derives identity from CN).
type certBundle struct {
	dir string

	caPath         string // CA certificate (PEM)
	serverCertPath string // broker server cert (PEM, signed by CA)
	serverKeyPath  string // broker server key  (PEM)
}

// agentCert returns paths to a freshly-generated client cert+key signed
// by the bundle's CA, with CN == clusterID. Used by the agent client so
// the broker's CN-based ExtractClusterID returns clusterID for every
// request originating from this keypair.
type agentCert struct {
	certPath string
	keyPath  string
}

// newCertBundle generates the CA + server keypair and writes them to a
// temp dir. The CA's private key is held in-memory for issuing client
// certs later via issueAgentCert.
type bundleBuilder struct {
	dir       string
	caCert    *x509.Certificate
	caKey     *ecdsa.PrivateKey
	caCertPEM []byte
}

func newBundleBuilder(dir string) (*bundleBuilder, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ca key: %w", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("ca cert: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}
	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	return &bundleBuilder{
		dir:       dir,
		caCert:    caCert,
		caKey:     caKey,
		caCertPEM: caCertPEM,
	}, nil
}

// issueServer mints a server cert with SANs covering 127.0.0.1, ::1 and
// localhost; that covers any address the test listener might bind to.
// Returns the certBundle ready for use by the broker Runnable.
func (b *bundleBuilder) issueServer() (*certBundle, error) {
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("server key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "broker.test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost", "broker.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, b.caCert, &srvKey.PublicKey, b.caKey)
	if err != nil {
		return nil, fmt.Errorf("server cert: %w", err)
	}

	caPath := filepath.Join(b.dir, "ca.pem")
	srvCertPath := filepath.Join(b.dir, "server.pem")
	srvKeyPath := filepath.Join(b.dir, "server-key.pem")

	if err := os.WriteFile(caPath, b.caCertPEM, 0o600); err != nil {
		return nil, err
	}
	srvCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(srvCertPath, srvCertPEM, 0o600); err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		return nil, err
	}
	srvKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(srvKeyPath, srvKeyPEM, 0o600); err != nil {
		return nil, err
	}

	return &certBundle{
		dir:            b.dir,
		caPath:         caPath,
		serverCertPath: srvCertPath,
		serverKeyPath:  srvKeyPath,
	}, nil
}

// issueAgentCert issues an agent client cert with CN == clusterID so
// the broker's ExtractClusterID returns clusterID for every request
// using the resulting keypair.
func (b *bundleBuilder) issueAgentCert(clusterID string, serial int64) (*agentCert, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("agent key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: clusterID},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, b.caCert, &key.PublicKey, b.caKey)
	if err != nil {
		return nil, fmt.Errorf("agent cert: %w", err)
	}
	certPath := filepath.Join(b.dir, fmt.Sprintf("agent-%s.pem", clusterID))
	keyPath := filepath.Join(b.dir, fmt.Sprintf("agent-%s-key.pem", clusterID))
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	return &agentCert{certPath: certPath, keyPath: keyPath}, nil
}
