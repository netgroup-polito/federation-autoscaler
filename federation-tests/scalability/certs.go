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

package main

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
)

// agentIdentity is the (clusterID, cert, key) triple one logical agent
// authenticates with. clusterID MUST equal the certificate's Subject.CommonName
// — the Broker's ClusterIDMiddleware derives the caller's identity from the
// verified mTLS leaf cert and every handler cross-checks it against
// body.clusterId (internal/broker/api/middleware.go, advertisement.go,
// heartbeat.go).
type agentIdentity struct {
	ClusterID string
	CertFile  string
	KeyFile   string
}

// resolveIdentities builds the ordered list of provider and consumer
// identities for this run, either from a generate-test-certs.sh output
// directory (one cert per logical agent) or from a single shared
// --tls-cert/--tls-key pair (Validate() already restricted that path to
// <=1 provider and <=1 consumer).
func resolveIdentities(cfg *Config) (providers, consumers []agentIdentity, caFile string, err error) {
	if cfg.CertsDir != "" {
		caFile = filepath.Join(cfg.CertsDir, "ca.crt")
		if _, statErr := os.Stat(caFile); statErr != nil {
			return nil, nil, "", fmt.Errorf("certs-dir %q: %w (run generate-test-certs.sh first)", cfg.CertsDir, statErr)
		}
		for i := 1; i <= cfg.Providers; i++ {
			id := fmt.Sprintf("%s-provider-%03d", clusterIDPrefix, i)
			cert := filepath.Join(cfg.CertsDir, id+".crt")
			key := filepath.Join(cfg.CertsDir, id+".key")
			if _, statErr := os.Stat(cert); statErr != nil {
				return nil, nil, "", fmt.Errorf("provider cert %q: %w (generate-test-certs.sh --providers must be >= %d)", cert, statErr, cfg.Providers)
			}
			providers = append(providers, agentIdentity{ClusterID: id, CertFile: cert, KeyFile: key})
		}
		for i := 1; i <= cfg.Consumers; i++ {
			id := fmt.Sprintf("%s-consumer-%03d", clusterIDPrefix, i)
			cert := filepath.Join(cfg.CertsDir, id+".crt")
			key := filepath.Join(cfg.CertsDir, id+".key")
			if _, statErr := os.Stat(cert); statErr != nil {
				return nil, nil, "", fmt.Errorf("consumer cert %q: %w (generate-test-certs.sh --consumers must be >= %d)", cert, statErr, cfg.Consumers)
			}
			consumers = append(consumers, agentIdentity{ClusterID: id, CertFile: cert, KeyFile: key})
		}
		return providers, consumers, caFile, nil
	}

	// Shared single-cert mode: the logical agent's ClusterID MUST be the
	// cert's own CN (the Broker rejects any body.clusterId that doesn't
	// match), so it is read from the certificate rather than synthesised.
	cn, err := certCommonName(cfg.TLSCert)
	if err != nil {
		return nil, nil, "", err
	}
	caFile = cfg.TLSCA
	id := agentIdentity{ClusterID: cn, CertFile: cfg.TLSCert, KeyFile: cfg.TLSKey}
	if cfg.Providers == 1 {
		providers = append(providers, id)
	}
	if cfg.Consumers == 1 {
		consumers = append(consumers, id)
	}
	return providers, consumers, caFile, nil
}

// certCommonName extracts Subject.CommonName from a PEM-encoded certificate
// file, matching how internal/broker/api/tls.go:ExtractClusterID reads it
// off the verified mTLS peer certificate.
func certCommonName(path string) (string, error) {
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

// newAgentClient builds the same mTLS HTTP client a real Provider/Consumer
// Agent uses (internal/agent/client), scoped to one logical agent's
// identity. ServerName, when set, overrides the host parsed from BrokerURL
// so the Broker's server certificate — whose SANs cover only the in-cluster
// Service DNS names (config/broker/certmanager.yaml) — still verifies when
// dialed via `kubectl port-forward` to 127.0.0.1/localhost. It is a
// variable only so tests can point the load generators at an httptest server.
var newAgentClient = func(cfg *Config, id agentIdentity, caFile string) (*agentclient.Client, error) {
	return agentclient.New(agentclient.Options{
		BrokerURL: cfg.BrokerURL,
		TLS: agentclient.TLSConfig{
			CertFile:     id.CertFile,
			KeyFile:      id.KeyFile,
			BrokerCAFile: caFile,
			ServerName:   cfg.BrokerServerName,
		},
		RequestTimeout: cfg.RequestTimeout,
		MaxRetries:     cfg.ClientMaxRetries,
	})
}
