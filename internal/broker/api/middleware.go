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
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// -----------------------------------------------------------------------------
// Middleware composition
// -----------------------------------------------------------------------------

// Middleware is the standard "decorator" signature used throughout this
// package: every layer takes the next http.Handler and returns a wrapping
// handler.
type Middleware func(http.Handler) http.Handler

// chain wraps next with the given middlewares such that the FIRST element
// of mws ends up outermost on the wire. Example:
//
//	chain(handler, recover, requestID, logger)
//
// produces  recover( requestID( logger( handler ) ) ) — i.e. a request first
// passes through recover, then requestID, then logger, before reaching the
// real handler.
func chain(next http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		next = mws[i](next)
	}
	return next
}

// Middleware returns a single http.Handler that wraps the Server's bare
// route mux (Handler()) with the canonical Broker middleware chain:
//
//  1. Recover         — convert panics into a 500 ErrorResponse.
//  2. RequestID       — assign / propagate X-Request-Id.
//  3. Logger          — emit a structured log line per request.
//  4. ClusterID       — pull the caller cluster from the mTLS leaf cert.
//  5. RateLimit       — per-cluster token bucket (10 burst, 5 sustained rps).
//
// Steps 1–3 wrap step 4 so authentication failures and rate-limited
// rejections still get logged with a requestId.
func (s *Server) MiddlewareChain(handler http.Handler) http.Handler {
	return chain(handler,
		s.RecoverMiddleware,
		s.RequestIDMiddleware,
		s.LoggerMiddleware,
		s.ClusterIDMiddleware,
		s.RateLimitMiddleware(),
	)
}

// -----------------------------------------------------------------------------
// 1. Recover
// -----------------------------------------------------------------------------

// RecoverMiddleware converts panics from any downstream handler into a 500
// ErrorResponse. It logs the stack trace at error level so on-call engineers
// can correlate the failure with the originating request via X-Request-Id.
func (s *Server) RecoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error(nil, "panic in handler",
					"requestId", r.Header.Get(HeaderRequestID),
					"panic", rec,
					"stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, ErrorResponse{
					Code:      ErrCodeInternalError,
					Message:   "internal server error",
					RequestID: r.Header.Get(HeaderRequestID),
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// -----------------------------------------------------------------------------
// 2. Request-ID
// -----------------------------------------------------------------------------

// RequestIDMiddleware ensures every request has a non-empty X-Request-Id by
// generating a UUID v4 when the client did not supply one. The value is
// echoed back in the response header so the agent can correlate retries.
func (s *Server) RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if id == "" {
			id = uuid.NewString()
			r.Header.Set(HeaderRequestID, id)
		}
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r)
	})
}

// -----------------------------------------------------------------------------
// 3. Structured request logger
// -----------------------------------------------------------------------------

// statusRecorder is a tiny wrapper that captures the status code so the
// logger middleware can record it after the inner handler returns.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// LoggerMiddleware emits one structured line per request at the V(0) level
// containing method, path, status, duration_ms, requestId, and clusterId
// (if already extracted by ClusterIDMiddleware).
func (s *Server) LoggerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		// Build a request-scoped logger and stash it on the context so
		// handlers don't have to re-stamp the same fields.
		reqLog := s.log.WithValues("requestId", r.Header.Get(HeaderRequestID))
		next.ServeHTTP(rec, r.WithContext(newContextWithLogger(r.Context(), reqLog)))

		reqLog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"durationMs", time.Since(start).Milliseconds(),
			"clusterId", ClusterIDFromContext(r.Context()))
	})
}

// loggerContextKey is unexported; access through {New,From}ContextLogger.
type loggerContextKey struct{}

// newContextWithLogger returns a ctx that carries log.
func newContextWithLogger(ctx context.Context, log logr.Logger) context.Context {
	return context.WithValue(ctx, loggerContextKey{}, log)
}

// LoggerFromContext returns the request-scoped logger, or a discarding
// logger if none has been set (e.g. in unit tests that bypass middleware).
func LoggerFromContext(ctx context.Context) logr.Logger {
	if v, ok := ctx.Value(loggerContextKey{}).(logr.Logger); ok {
		return v
	}
	return logr.Discard()
}

// -----------------------------------------------------------------------------
// 4. Cluster-ID extraction
// -----------------------------------------------------------------------------

// ClusterIDMiddleware enforces docs/design.md §7.0: the calling cluster's
// identity is the CN of its mTLS leaf certificate. Any request without a
// verified peer cert (e.g. someone stripped TLS in front of the Broker) is
// rejected with 401 Unauthenticated.
func (s *Server) ClusterIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clusterID, err := ExtractClusterID(r.TLS)
		if err != nil {
			writeError(w, http.StatusUnauthorized, ErrorResponse{
				Code:      ErrCodeUnauthenticated,
				Message:   err.Error(),
				RequestID: r.Header.Get(HeaderRequestID),
			})
			return
		}
		ctx := NewContextWithClusterID(r.Context(), clusterID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// -----------------------------------------------------------------------------
// 5. Per-cluster rate limiter
// -----------------------------------------------------------------------------

// RateLimitConfig tunes the token bucket applied per cluster ID. Defaults
// follow docs/design.md §7.3.6: 10 burst tokens, 5 sustained tokens/sec —
// enough for the 5 s instruction poll plus occasional bursts (heartbeat,
// reservation), tight enough to throttle a misbehaving agent.
type RateLimitConfig struct {
	BurstTokens int     // bucket size
	RatePerSec  float64 // refill rate
}

// DefaultRateLimitConfig returns the values mandated by §7.3.6.
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{BurstTokens: 10, RatePerSec: 5}
}

// RateLimitMiddleware returns a middleware that enforces RateLimitConfig per
// cluster ID. ClusterIDMiddleware MUST run before this layer; if no cluster
// ID is present (shouldn't happen in production) we let the request through
// unthrottled because rejecting it would mask a real auth bug.
func (s *Server) RateLimitMiddleware() Middleware {
	cfg := DefaultRateLimitConfig()
	limiters := &sync.Map{} // map[clusterID]*rate.Limiter

	getLimiter := func(clusterID string) *rate.Limiter {
		if v, ok := limiters.Load(clusterID); ok {
			return v.(*rate.Limiter)
		}
		// Race-tolerant: LoadOrStore returns whichever value won.
		fresh := rate.NewLimiter(rate.Limit(cfg.RatePerSec), cfg.BurstTokens)
		actual, _ := limiters.LoadOrStore(clusterID, fresh)
		return actual.(*rate.Limiter)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clusterID := ClusterIDFromContext(r.Context())
			if clusterID == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !getLimiter(clusterID).Allow() {
				writeError(w, http.StatusTooManyRequests, ErrorResponse{
					Code:      ErrCodeTooManyRequests,
					Message:   "per-cluster rate limit exceeded",
					Details:   map[string]any{"clusterId": clusterID},
					RequestID: r.Header.Get(HeaderRequestID),
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
