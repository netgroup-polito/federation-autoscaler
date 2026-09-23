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

package testlib

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
)

// Identity is the (clusterID, cert path, key path) triple for one logical
// agent. ClusterID MUST equal the certificate's Subject.CommonName — the
// Broker's ClusterIDMiddleware derives the caller identity from the verified
// mTLS leaf cert.
type Identity struct {
	ClusterID string
	CertFile  string
	KeyFile   string
}

// CertFingerprint returns the hex-encoded SHA-256 fingerprint of the
// DER-encoded leaf certificate. Useful for logging identity without
// exposing the certificate contents (security constraint).
func (id Identity) CertFingerprint() (string, error) {
	raw, err := os.ReadFile(id.CertFile)
	if err != nil {
		return "", fmt.Errorf("read cert %q: %w", id.CertFile, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return "", fmt.Errorf("cert %q: no PEM block found", id.CertFile)
	}
	h := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(h[:]), nil
}

// ResolveConsumerIdentity loads the consumer identity from a certs directory
// produced by generate-test-certs.sh. The consumer cert is expected at
// <certsDir>/<prefix>-consumer-001.{crt,key} and the CA at <certsDir>/ca.crt.
func ResolveConsumerIdentity(certsDir, prefix string) (id Identity, caFile string, err error) {
	caFile = filepath.Join(certsDir, "ca.crt")
	if _, statErr := os.Stat(caFile); statErr != nil {
		return Identity{}, "", fmt.Errorf("certs-dir %q: %w (run generate-test-certs.sh first)", certsDir, statErr)
	}
	name := fmt.Sprintf("%s-consumer-001", prefix)
	cert := filepath.Join(certsDir, name+".crt")
	key := filepath.Join(certsDir, name+".key")
	if _, statErr := os.Stat(cert); statErr != nil {
		return Identity{}, "", fmt.Errorf("consumer cert %q: %w", cert, statErr)
	}
	cn, err := CertCommonName(cert)
	if err != nil {
		return Identity{}, "", err
	}
	return Identity{ClusterID: cn, CertFile: cert, KeyFile: key}, caFile, nil
}

// ResolveIdentityFromFiles loads an identity from explicit cert/key paths.
// The ClusterID is extracted from the cert's Subject.CommonName.
func ResolveIdentityFromFiles(certFile, keyFile, caFile string) (Identity, error) {
	cn, err := CertCommonName(certFile)
	if err != nil {
		return Identity{}, err
	}
	return Identity{ClusterID: cn, CertFile: certFile, KeyFile: keyFile}, nil
}

// CertCommonName extracts Subject.CommonName from a PEM-encoded certificate,
// matching how internal/broker/api/tls.go:ExtractClusterID reads it.
func CertCommonName(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read cert %q: %w", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return "", fmt.Errorf("cert %q: no PEM block found", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("cert %q: %w", path, err)
	}
	if cert.Subject.CommonName == "" {
		return "", fmt.Errorf("cert %q: Subject.CommonName is empty", path)
	}
	return cert.Subject.CommonName, nil
}

// NewBrokerClient builds the same mTLS HTTP client a real agent uses
// (internal/agent/client), scoped to one identity.
func NewBrokerClient(brokerURL, serverName string, id Identity, caFile string) (*agentclient.Client, error) {
	return agentclient.New(agentclient.Options{
		BrokerURL: brokerURL,
		TLS: agentclient.TLSConfig{
			CertFile:     id.CertFile,
			KeyFile:      id.KeyFile,
			BrokerCAFile: caFile,
			ServerName:   serverName,
		},
		RequestTimeout: 10 * time.Second,
		MaxRetries:     3,
	})
}
