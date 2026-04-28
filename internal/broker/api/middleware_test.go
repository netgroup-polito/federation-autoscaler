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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
)

// newTestServer returns a Server instance suitable for middleware testing —
// no Kubernetes client is required because middleware never reaches CRDs.
// The constructor's required-field panic is bypassed on purpose: we want
// to exercise the wrapping logic in isolation.
func newTestServer() *Server {
	return &Server{
		log:       logr.Discard(),
		namespace: "test-ns",
		consumers: NewConsumerRegistry(),
	}
}

// TestRecoverMiddleware verifies a panic in a handler turns into a 500
// ErrorResponse with the InternalError code (no leak of the panic value).
func TestRecoverMiddleware(t *testing.T) {
	s := newTestServer()
	h := s.RecoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/x", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want %d", rr.Code, http.StatusInternalServerError)
	}
	var body ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if body.Code != ErrCodeInternalError {
		t.Errorf("body.Code: got %q, want %q", body.Code, ErrCodeInternalError)
	}
}

// TestRequestIDMiddleware checks that
//   - a missing X-Request-Id is generated and echoed back, and
//   - a client-supplied X-Request-Id is preserved on the response.
func TestRequestIDMiddleware(t *testing.T) {
	s := newTestServer()
	h := s.RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Inner handler reads from request header to assert it was filled in.
		if r.Header.Get(HeaderRequestID) == "" {
			t.Errorf("inner handler: request header %q is empty", HeaderRequestID)
		}
		w.WriteHeader(http.StatusOK)
	}))

	t.Run("generated when missing", func(t *testing.T) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/x", nil))
		if got := rr.Header().Get(HeaderRequestID); got == "" {
			t.Errorf("response header %q is empty", HeaderRequestID)
		}
	})

	t.Run("preserved when supplied", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set(HeaderRequestID, "client-supplied-id")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if got := rr.Header().Get(HeaderRequestID); got != "client-supplied-id" {
			t.Errorf("got %q, want %q", got, "client-supplied-id")
		}
	})
}

// TestClusterIDMiddleware ensures a missing peer certificate yields 401.
func TestClusterIDMiddleware_Missing(t *testing.T) {
	s := newTestServer()
	called := false
	h := s.ClusterIDMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/x", nil))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rr.Code)
	}
	if called {
		t.Errorf("inner handler must not run when auth fails")
	}
}

// TestClusterIDMiddleware_Happy ensures a valid TLS state propagates the CN
// into the request context.
func TestClusterIDMiddleware_Happy(t *testing.T) {
	s := newTestServer()
	var seen string
	h := s.ClusterIDMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = ClusterIDFromContext(r.Context())
	}))

	req := httptest.NewRequest("GET", "/x", nil)
	req.TLS = &tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{
			{Subject: pkix.Name{CommonName: consumerCluster}},
		}},
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if seen != consumerCluster {
		t.Errorf("clusterID in context: got %q, want %q", seen, consumerCluster)
	}
}

// TestRateLimitMiddleware verifies that exceeding the burst returns 429
// while a different cluster is still allowed (per-cluster bucketing).
func TestRateLimitMiddleware(t *testing.T) {
	s := newTestServer()
	mw := s.RateLimitMiddleware()
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	makeReq := func(clusterID string) *http.Request {
		req := httptest.NewRequest("GET", "/x", nil)
		ctx := NewContextWithClusterID(context.Background(), clusterID)
		return req.WithContext(ctx)
	}

	burst := DefaultRateLimitConfig().BurstTokens

	// Drain burst for cluster-a — every call should succeed.
	for i := 0; i < burst; i++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, makeReq("cluster-a"))
		if rr.Code != http.StatusOK {
			t.Fatalf("burst[%d]: got %d, want 200", i, rr.Code)
		}
	}

	// Next call from cluster-a is throttled.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, makeReq("cluster-a"))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("post-burst: got %d, want 429", rr.Code)
	}

	// Different cluster still has its own bucket.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, makeReq("cluster-b"))
	if rr.Code != http.StatusOK {
		t.Errorf("cluster-b: got %d, want 200 (independent bucket)", rr.Code)
	}
}
