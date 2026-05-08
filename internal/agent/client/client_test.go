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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// newTestClient returns a Client wired to talk to an httptest.Server. It
// rewrites the BrokerURL to the server's https-looking address (we lie
// about the scheme to satisfy the "https only" guard in New) and injects
// the server's transport so no real TLS happens.
//
// Tests that exercise retries set MaxRetries / InitialBackoff explicitly;
// otherwise they get the production defaults, which would slow tests
// down only when retries are actually triggered.
func newTestClient(t *testing.T, srv *httptest.Server, opts Options) *Client {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Force the scheme to https so New's guard passes; the injected
	// Transport handles the actual round-trip via the test server.
	opts.BrokerURL = "https://" + u.Host
	opts.Transport = rewriteScheme{base: srv.Client().Transport, target: u}
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// rewriteScheme forwards a request whose URL the test client built with
// scheme=https to the underlying httptest.Server's actual http URL. It
// keeps tests free of real TLS while still exercising New's https-only
// guard.
type rewriteScheme struct {
	base   http.RoundTripper
	target *url.URL
}

func (rt rewriteScheme) RoundTrip(req *http.Request) (*http.Response, error) {
	cp := *req.URL
	cp.Scheme = rt.target.Scheme
	cp.Host = rt.target.Host
	r2 := req.Clone(req.Context())
	r2.URL = &cp
	return rt.base.RoundTrip(r2)
}

func TestNew_Validation(t *testing.T) {
	cases := []struct {
		name   string
		opts   Options
		errSub string
	}{
		{"missing url", Options{}, "BrokerURL is required"},
		{"non-https", Options{BrokerURL: "http://broker"}, "must use https"},
		{"missing host", Options{BrokerURL: "https://"}, "no host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("expected error containing %q, got %v", tc.errSub, err)
			}
		})
	}
}

func TestDo_HappyPath_DecodesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(map[string]string{"hello": "world"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, Options{})
	var out map[string]string
	if err := c.do(context.Background(), http.MethodGet, "/api/v1/probe", nil, &out, requestOptions{}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if out["hello"] != "world" {
		t.Fatalf("body decode mismatch: %v", out)
	}
}

func TestDo_InjectsRequestID_WhenAbsent(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(brokerapi.HeaderRequestID)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, Options{})
	if err := c.do(context.Background(), http.MethodGet, "/probe", nil, nil, requestOptions{}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if seen == "" {
		t.Fatal("expected X-Request-Id to be auto-generated, got empty")
	}
}

func TestDo_PropagatesProvidedRequestID(t *testing.T) {
	want := "fixed-correlation-id"
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(brokerapi.HeaderRequestID)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, Options{})
	if err := c.do(context.Background(), http.MethodGet, "/probe", nil, nil, requestOptions{requestID: want}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if seen != want {
		t.Fatalf("expected X-Request-Id %q, got %q", want, seen)
	}
}

func TestDo_AttachesReservationID(t *testing.T) {
	const want = "res-abc"
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(brokerapi.HeaderReservationID)
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, Options{})
	if err := c.do(context.Background(), http.MethodPost, "/api/v1/reservations", map[string]string{"x": "y"}, &struct{}{}, requestOptions{
		reservationID: want,
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if seen != want {
		t.Fatalf("expected X-Reservation-Id %q, got %q", want, seen)
	}
}

func TestDo_RetriesTransient_ThenSucceeds(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, Options{
		MaxRetries:     5,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	})
	var out map[string]any
	err := c.do(context.Background(), http.MethodGet, "/probe", nil, &out, requestOptions{idempotent: true})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

func TestDo_NonIdempotentDoesNotRetry(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, Options{MaxRetries: 5, InitialBackoff: time.Millisecond})
	err := c.do(context.Background(), http.MethodPost, "/probe", nil, nil, requestOptions{ /* idempotent: false */ })
	if !IsTransient(err) {
		t.Fatalf("expected transient error, got %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected exactly 1 attempt for non-idempotent request, got %d", got)
	}
}

func TestDo_GivesUpAfterMaxRetries(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, Options{MaxRetries: 2, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond})
	err := c.do(context.Background(), http.MethodGet, "/probe", nil, nil, requestOptions{idempotent: true})
	if !IsTransient(err) {
		t.Fatalf("expected transient error after exhausting retries, got %v", err)
	}
	// MaxRetries=2 means up to 3 attempts total (1 initial + 2 retries).
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

func TestDo_ClassifiesErrorBody(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		code      brokerapi.ErrorCode
		predicate func(error) bool
	}{
		{"bad request", http.StatusBadRequest, brokerapi.ErrCodeInvalidRequest, IsBadRequest},
		{"forbidden", http.StatusForbidden, brokerapi.ErrCodeForbidden, IsForbidden},
		{"not found", http.StatusNotFound, brokerapi.ErrCodeNotFound, IsNotFound},
		{"conflict", http.StatusConflict, brokerapi.ErrCodeConflict, IsConflict},
		{"precondition", http.StatusPreconditionFailed, brokerapi.ErrCodeServiceUnavailable, IsPreconditionFailed},
		{"too many", http.StatusTooManyRequests, brokerapi.ErrCodeTooManyRequests, IsTooManyRequests},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(brokerapi.ErrorResponse{
					Code: tc.code, Message: tc.name, RequestID: "req-1",
				})
			}))
			defer srv.Close()

			c := newTestClient(t, srv, Options{MaxRetries: 0, InitialBackoff: time.Millisecond})
			err := c.do(context.Background(), http.MethodGet, "/probe", nil, nil, requestOptions{idempotent: true})
			if !tc.predicate(err) {
				t.Fatalf("predicate did not match for %s: err=%v", tc.name, err)
			}
			var typed *Error
			if !errors.As(err, &typed) {
				t.Fatalf("expected *Error, got %T", err)
			}
			if typed.Status != tc.status {
				t.Errorf("status: want %d, got %d", tc.status, typed.Status)
			}
			if typed.Code != tc.code {
				t.Errorf("code: want %s, got %s", tc.code, typed.Code)
			}
			if typed.RequestID != "req-1" {
				t.Errorf("request id: want req-1, got %q", typed.RequestID)
			}
		})
	}
}

func TestDo_HandlesEmptyErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, Options{MaxRetries: 0})
	err := c.do(context.Background(), http.MethodGet, "/probe", nil, nil, requestOptions{idempotent: true})
	if !IsBadRequest(err) {
		t.Fatalf("expected bad request, got %v", err)
	}
	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if typed.Message == "" {
		t.Errorf("expected non-empty fallback message, got empty")
	}
}

func TestDo_ContextCancelDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, Options{
		MaxRetries:     5,
		InitialBackoff: 200 * time.Millisecond,
		MaxBackoff:     200 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := c.do(ctx, http.MethodGet, "/probe", nil, nil, requestOptions{idempotent: true})
	dur := time.Since(start)
	if err == nil {
		t.Fatal("expected error after cancellation")
	}
	// The first attempt fails with 503, then we sleep for InitialBackoff
	// (200ms). Cancel fires at ~20ms so we should return well under 200ms.
	if dur > 150*time.Millisecond {
		t.Errorf("cancellation should short-circuit backoff; took %v", dur)
	}
}
