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

// Package client implements the agent-side mTLS HTTP client that every
// Provider and Consumer Agent uses to talk to the Broker. It mirrors the
// server-side mTLS configuration in internal/broker/api/tls.go: the Broker
// derives the calling cluster's identity from the CN of the leaf client
// certificate, so the cert + key + CA bundle the agent ships with this
// package's BuildClientTLSConfig MUST be issued by the same authority the
// Broker trusts.
package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// TLSConfig is the file-backed configuration the agent uses to dial the
// Broker over mTLS (docs/design.md §10.1). All three paths are mandatory:
// the Broker rejects any agent that does not present a client certificate
// signed by the CA the Broker is configured to trust, and the agent in
// turn refuses to dial a Broker whose server certificate cannot be
// verified against BrokerCAFile.
type TLSConfig struct {
	// CertFile is the PEM-encoded client certificate the agent presents
	// during the TLS handshake. Its Subject.CommonName MUST equal the
	// agent's --cluster-id (see cmd/agent/main.go), since that is what
	// the Broker extracts in middleware to derive the caller identity.
	CertFile string

	// KeyFile is the PEM-encoded private key matching CertFile.
	KeyFile string

	// BrokerCAFile is the PEM-encoded CA bundle that signed the Broker's
	// server certificate. Trust is anchored here and nowhere else.
	BrokerCAFile string

	// MinVersion overrides the floor for negotiated TLS versions.
	// Defaults to TLS 1.3 to match the Broker's posture.
	MinVersion uint16

	// ServerName overrides the hostname verified against the Broker's
	// server certificate SAN list. Defaults to the host parsed from the
	// Broker URL by the caller; tests use this to override SAN matching
	// when dialing 127.0.0.1.
	ServerName string
}

// Validate returns an error if any of the mandatory paths is missing.
// File existence on disk is left for BuildClientTLSConfig to surface so
// helpful "no such file" errors point at the actual offending path.
func (c TLSConfig) Validate() error {
	switch {
	case c.CertFile == "":
		return fmt.Errorf("tls: cert-file is required")
	case c.KeyFile == "":
		return fmt.Errorf("tls: key-file is required")
	case c.BrokerCAFile == "":
		return fmt.Errorf("tls: broker-ca-file is required (mTLS is mandatory)")
	}
	return nil
}

// BuildClientTLSConfig loads the agent client keypair and the broker CA
// bundle and returns a *tls.Config wired for mTLS:
//
//   - Certificates = the agent's client keypair, presented on every call
//   - RootCAs      = the bundle in BrokerCAFile (broker server-cert anchor)
//   - MinVersion   = TLS 1.3 unless the caller overrode it
//
// Hot-reload of certificates is intentionally out of scope: cert-manager
// rotates the underlying Secret and the pod is restarted, mirroring the
// broker's behaviour.
func (c TLSConfig) BuildClientTLSConfig() (*tls.Config, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	clientCert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tls: loading client keypair (cert=%q key=%q): %w",
			c.CertFile, c.KeyFile, err)
	}

	caBytes, err := os.ReadFile(c.BrokerCAFile)
	if err != nil {
		return nil, fmt.Errorf("tls: reading broker-ca file %q: %w", c.BrokerCAFile, err)
	}
	rootCAs := x509.NewCertPool()
	if !rootCAs.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("tls: broker-ca file %q contains no PEM certificates", c.BrokerCAFile)
	}

	min := c.MinVersion
	if min == 0 {
		min = tls.VersionTLS13
	}

	return &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      rootCAs,
		MinVersion:   min,
		ServerName:   c.ServerName,
	}, nil
}
