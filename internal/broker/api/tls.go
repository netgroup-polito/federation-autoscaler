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

package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// TLSConfig is the file-backed configuration the Broker uses to terminate
// mTLS connections from agents (docs/design.md §10.1). All three paths are
// mandatory: the Broker rejects any agent that does not present a client
// certificate signed by ClientCAFile.
type TLSConfig struct {
	// CertFile is the PEM-encoded server certificate served on the TLS
	// handshake. Its SAN list MUST cover the public hostname agents use.
	CertFile string

	// KeyFile is the PEM-encoded private key matching CertFile.
	KeyFile string

	// ClientCAFile is the PEM-encoded CA bundle that signed every agent's
	// client certificate. Trust is anchored here and nowhere else.
	ClientCAFile string

	// MinVersion overrides the floor for negotiated TLS versions. Defaults
	// to TLS 1.3 — older versions are rejected to avoid CBC / RC4 / SHA-1
	// pitfalls and to match upstream k8s-resource-brokering's posture.
	MinVersion uint16
}

// Validate returns an error if any of the mandatory paths is missing.
// Non-existence on disk is left for BuildServerTLSConfig to surface so
// helpful "no such file" errors point at the actual offending path.
func (c TLSConfig) Validate() error {
	switch {
	case c.CertFile == "":
		return fmt.Errorf("tls: cert-file is required")
	case c.KeyFile == "":
		return fmt.Errorf("tls: key-file is required")
	case c.ClientCAFile == "":
		return fmt.Errorf("tls: client-ca-file is required (mTLS is mandatory)")
	}
	return nil
}

// BuildServerTLSConfig loads the server certificate and the client-CA bundle
// and returns a *tls.Config wired for mTLS:
//
//   - ClientAuth = RequireAndVerifyClientCert (no anonymous traffic)
//   - ClientCAs  = the bundle in ClientCAFile
//   - MinVersion = TLS 1.3 unless the caller overrode it
//
// Hot-reload of certificates is intentionally out of scope: cert-manager
// rotates the underlying Secret and the pod is restarted, which is what the
// upstream k8s-resource-brokering Broker also does.
func (c TLSConfig) BuildServerTLSConfig() (*tls.Config, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	serverCert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tls: loading server keypair (cert=%q key=%q): %w",
			c.CertFile, c.KeyFile, err)
	}

	caBytes, err := os.ReadFile(c.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("tls: reading client-ca file %q: %w", c.ClientCAFile, err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("tls: client-ca file %q contains no PEM certificates", c.ClientCAFile)
	}

	min := c.MinVersion
	if min == 0 {
		min = tls.VersionTLS13
	}

	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		MinVersion:   min,
	}, nil
}

// -----------------------------------------------------------------------------
// Cluster-ID extraction
// -----------------------------------------------------------------------------
//
// docs/design.md §7.0 mandates that the Broker derive the calling cluster's
// identity from the leaf client certificate's Common Name. mTLS guarantees
// the chain is verified before any application code runs; the helpers below
// just locate and return that already-trusted value.

// ExtractClusterID returns the CN of the leaf client certificate in the
// already-verified chain. It is meant to be called from middleware on
// requests served by a tls.Config built via BuildServerTLSConfig (which
// uses RequireAndVerifyClientCert, so a valid chain is guaranteed).
//
// It returns an error rather than panicking on missing/empty CN so the
// caller can choose between 401 Unauthenticated (no peer certs) and 403
// Forbidden (peer cert present but no usable CN).
func ExtractClusterID(state *tls.ConnectionState) (string, error) {
	if state == nil {
		return "", fmt.Errorf("tls: no connection state on request")
	}
	if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return "", fmt.Errorf("tls: no verified peer certificate")
	}
	leaf := state.VerifiedChains[0][0]
	cn := leaf.Subject.CommonName
	if cn == "" {
		return "", fmt.Errorf("tls: peer certificate Subject.CN is empty")
	}
	return cn, nil
}

// -----------------------------------------------------------------------------
// Cluster-ID propagation through context
// -----------------------------------------------------------------------------

// contextKey is unexported so package consumers cannot accidentally collide
// with our context values.
type contextKey int

const (
	clusterIDContextKey contextKey = iota
)

// NewContextWithClusterID returns a copy of ctx that carries clusterID.
// Used by the cluster-ID middleware in middleware.go (step 4d).
func NewContextWithClusterID(ctx context.Context, clusterID string) context.Context {
	return context.WithValue(ctx, clusterIDContextKey, clusterID)
}

// ClusterIDFromContext returns the cluster ID stored by
// NewContextWithClusterID, or "" if no value has been threaded through.
func ClusterIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(clusterIDContextKey).(string)
	return v
}
