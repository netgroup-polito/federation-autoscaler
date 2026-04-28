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
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/netgroup-polito/federation-autoscaler/internal/broker/chunk"
)

// Compile-time interface checks: Runnable plugs into the controller-runtime
// manager exactly like the controllers do, and it opts INTO leader election
// so only the elected Broker pod serves agents (avoiding split-brain writes
// to ClusterAdvertisement / Reservation CRDs).
var (
	_ manager.Runnable               = (*Runnable)(nil)
	_ manager.LeaderElectionRunnable = (*Runnable)(nil)
)

// RunnableOptions bundles everything needed to spin up the Broker REST API
// inside a controller-runtime manager. Empty fields fall back to defaults.
type RunnableOptions struct {
	// BindAddress is the host:port the HTTPS listener binds to. Defaults
	// to ":8443" — well-known port for non-root mTLS services.
	BindAddress string

	// TLS holds the file paths for the server cert, key and client-CA
	// bundle. All three are mandatory; mTLS is non-negotiable per
	// docs/design.md §10.1.
	TLS TLSConfig

	// ShutdownTimeout caps how long Start will wait for in-flight requests
	// to drain when ctx is cancelled. Defaults to 15s.
	ShutdownTimeout time.Duration

	// Logger is used for setup messages. Defaults to ctrl.Log.WithName("broker-api").
	Logger logr.Logger

	// Client is the controller-runtime client passed to the Server. The
	// manager's cached client is the canonical choice; raw clients work too
	// but skip the cache.
	Client client.Client

	// Namespace is where every Broker-owned CR is stored. Required.
	Namespace string

	// Sizer is the policy used by handlers to split an advertisement into
	// chunks. Defaults to chunk.DefaultSizer.
	Sizer chunk.Sizer
}

// Runnable adapts the Broker's HTTP Server to the manager.Runnable interface
// so it shares the manager's lifecycle, leader election, signal handling,
// and shutdown sequencing with the CRD reconcilers in cmd/broker/main.go.
type Runnable struct {
	server   *Server
	httpSrv  *http.Server
	bindAddr string
	shutdown time.Duration
	log      logr.Logger
}

// NewRunnable validates opts, builds the *tls.Config, composes the Server's
// route mux with the middleware chain from middleware.go, and returns a
// Runnable ready to be passed to mgr.Add().
//
// Returning early on TLS errors is deliberate: a missing cert file should
// fail fast at startup rather than producing handshake errors at runtime.
func NewRunnable(opts RunnableOptions) (*Runnable, error) {
	tlsCfg, err := opts.TLS.BuildServerTLSConfig()
	if err != nil {
		return nil, fmt.Errorf("broker api: %w", err)
	}

	bind := opts.BindAddress
	if bind == "" {
		bind = ":8443"
	}
	shutdown := opts.ShutdownTimeout
	if shutdown <= 0 {
		shutdown = 15 * time.Second
	}
	log := opts.Logger
	if log.GetSink() == nil {
		log = ctrl.Log.WithName("broker-api")
	}

	srv := NewServer(ServerOptions{
		Logger:    log,
		Client:    opts.Client,
		Namespace: opts.Namespace,
		Sizer:     opts.Sizer,
	})

	httpSrv := &http.Server{
		Addr:      bind,
		Handler:   srv.MiddlewareChain(srv.Handler()),
		TLSConfig: tlsCfg,

		// Conservative timeouts; the longest-lived request is the 5s
		// instruction poll, so 30s read/write is generous.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	return &Runnable{
		server:   srv,
		httpSrv:  httpSrv,
		bindAddr: bind,
		shutdown: shutdown,
		log:      log,
	}, nil
}

// Server exposes the underlying *Server so tests and future wiring code can
// reach the route mux directly without re-running TLS plumbing.
func (r *Runnable) Server() *Server { return r.server }

// NeedLeaderElection returns true: only the elected Broker pod terminates
// agent traffic so duplicate-write races on ClusterAdvertisement and
// Reservation CRDs are impossible by construction.
func (*Runnable) NeedLeaderElection() bool { return true }

// Start blocks until ctx is cancelled, serving HTTPS in the meantime. Cert
// and key are passed empty because TLSConfig.Certificates is already loaded.
//
// On ctx.Done() it triggers a graceful shutdown bounded by ShutdownTimeout
// — in-flight handlers get a chance to finish, but a hung connection cannot
// keep the manager from terminating.
func (r *Runnable) Start(ctx context.Context) error {
	r.log.Info("starting broker api server",
		"bindAddress", r.bindAddr,
		"minTLSVersion", r.httpSrv.TLSConfig.MinVersion,
		"clientAuth", "RequireAndVerifyClientCert")

	serveErr := make(chan error, 1)
	go func() {
		// Empty cert/key paths: certs come from TLSConfig.
		err := r.httpSrv.ListenAndServeTLS("", "")
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		r.log.Info("draining broker api server", "shutdownTimeout", r.shutdown)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), r.shutdown)
		defer cancel()
		if err := r.httpSrv.Shutdown(shutdownCtx); err != nil {
			// Force-close on shutdown failure so the manager is not blocked.
			_ = r.httpSrv.Close()
			return fmt.Errorf("broker api: graceful shutdown: %w", err)
		}
		// Surface any latent ListenAndServe error captured during drain.
		if err := <-serveErr; err != nil {
			return fmt.Errorf("broker api: serve: %w", err)
		}
		return nil
	case err := <-serveErr:
		return fmt.Errorf("broker api: serve: %w", err)
	}
}
