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
	"crypto/x509/pkix"
	"testing"
)

// TestTLSConfig_Validate covers the three required paths and the happy
// case. We do NOT exercise BuildServerTLSConfig here because it touches
// the filesystem and crypto helpers — covered by the runnable's smoke
// tests if/when we add them.
func TestTLSConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     TLSConfig
		wantErr bool
	}{
		{name: "empty cert", cfg: TLSConfig{KeyFile: "k", ClientCAFile: "ca"}, wantErr: true},
		{name: "empty key", cfg: TLSConfig{CertFile: "c", ClientCAFile: "ca"}, wantErr: true},
		{name: "empty client ca", cfg: TLSConfig{CertFile: "c", KeyFile: "k"}, wantErr: true},
		{name: "happy path", cfg: TLSConfig{CertFile: "c", KeyFile: "k", ClientCAFile: "ca"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err=%v wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

// TestExtractClusterID covers all branches: nil state, empty chains, empty
// CN, populated CN.
func TestExtractClusterID(t *testing.T) {
	t.Run("nil state", func(t *testing.T) {
		if _, err := ExtractClusterID(nil); err == nil {
			t.Fatalf("expected error on nil state")
		}
	})

	t.Run("empty chains", func(t *testing.T) {
		if _, err := ExtractClusterID(&tls.ConnectionState{}); err == nil {
			t.Fatalf("expected error on empty verified chains")
		}
	})

	t.Run("empty CN", func(t *testing.T) {
		state := &tls.ConnectionState{
			VerifiedChains: [][]*x509.Certificate{{
				{Subject: pkix.Name{CommonName: ""}},
			}},
		}
		if _, err := ExtractClusterID(state); err == nil {
			t.Fatalf("expected error on empty CN")
		}
	})

	t.Run("happy", func(t *testing.T) {
		state := &tls.ConnectionState{
			VerifiedChains: [][]*x509.Certificate{{
				{Subject: pkix.Name{CommonName: "consumer-a"}},
			}},
		}
		got, err := ExtractClusterID(state)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "consumer-a" {
			t.Errorf("got %q, want %q", got, "consumer-a")
		}
	})
}

// TestClusterIDContext exercises the context round-trip.
func TestClusterIDContext(t *testing.T) {
	ctx := context.Background()
	if got := ClusterIDFromContext(ctx); got != "" {
		t.Errorf("expected empty cluster ID on bare context, got %q", got)
	}
	ctx = NewContextWithClusterID(ctx, "consumer-a")
	if got := ClusterIDFromContext(ctx); got != "consumer-a" {
		t.Errorf("got %q, want %q", got, "consumer-a")
	}
}
