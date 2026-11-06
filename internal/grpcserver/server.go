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

// Package grpcserver implements the
// clusterautoscaler.cloudprovider.v1.externalgrpc.CloudProvider gRPC
// service (docs/design.md §4.2): the consumer-cluster bridge Cluster
// Autoscaler dials via --cloud-provider=externalgrpc to scale virtual
// nodes that Liqo peers in from remote providers.
//
// Every RPC is implemented as a thin shell that translates CA's call
// into one or two HTTP requests against the co-located Consumer Agent's
// loopback REST API (step 9c). The gRPC server NEVER holds Broker
// credentials: it talks only to the local agent, which in turn talks
// to the Broker. See docs/design.md §4.2 for the chain.
//
// Substep 10a installs the skeleton: every RPC is an
// Unimplemented stub inherited from
// protos.UnimplementedCloudProviderServer. The Consumer Agent HTTP
// client (10b) and the per-RPC implementations (10c/10d/10e) plug in
// over the next few substeps.
package grpcserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"sigs.k8s.io/controller-runtime/pkg/log"

	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/agentclient"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/protos"
)

// DefaultShutdownTimeout caps how long Run waits for in-flight RPCs
// to drain when ctx is cancelled. CA's RPCs are short (single broker
// proxy round-trip), so 5 s is plenty.
const DefaultShutdownTimeout = 5 * time.Second

// Options bundles the construction-time settings of the gRPC server.
type Options struct {
	// BindAddress is the host:port the gRPC listener binds to.
	// Required.
	BindAddress string

	// TLS holds the file paths for the server cert, key, and
	// CA-bundle that signed CA's client cert. mTLS is non-negotiable —
	// CA's externalgrpc client always presents a cert.
	TLS TLSConfig

	// AgentClient is the typed loopback REST client for the
	// co-located Consumer Agent (step 10b). Every RPC that needs
	// broker-side state flows through it. Required once 10c lands.
	AgentClient *agentclient.Client

	// ShutdownTimeout overrides DefaultShutdownTimeout.
	ShutdownTimeout time.Duration

	// Logger is the structured logger setup messages log through.
	// Defaults to controller-runtime's logger named "grpcserver".
	Logger logr.Logger
}

// Server is the externalgrpc CloudProvider implementation. It embeds
// protos.UnimplementedCloudProviderServer so future proto additions
// (which gRPC v2 will allow without breaking us) keep the build green
// — and so substep 10a can ship with every RPC returning Unimplemented
// before 10c/10d/10e fill them in.
type Server struct {
	protos.UnimplementedCloudProviderServer

	opts    Options
	log     logr.Logger
	agent   *agentclient.Client
	grpcSrv *grpc.Server

	// listener is set by Run and read by Addr from other goroutines.
	lnMu     sync.Mutex
	listener net.Listener

	// nodeGroups read-through cache. CA calls NodeGroups + NodeGroupTargetSize +
	// NodeGroupTemplateNodeInfo for every group on every loop, all of which need
	// the broker's /nodegroups. Caching the response for a short TTL collapses
	// that per-loop burst into a single broker round-trip, keeping the consumer
	// under the broker's per-cluster rate limit (10 burst / 5 rps).
	ngMu      sync.Mutex
	ngCache   *brokerapi.NodeGroupListResponse
	ngCacheAt time.Time
}

// nodeGroupsCacheTTL bounds how long a GetNodeGroups result is reused across the
// read RPCs. It must stay well under CA's scan interval (~10 s) so
// NodeGroupTargetSize reflects a just-created reservation by the next loop, while
// still collapsing the intra-loop burst to one broker call.
const nodeGroupsCacheTTL = 2 * time.Second

// cachedNodeGroups returns the broker's node-group list, served from a short-TTL
// cache so CA's burst of read RPCs each loop costs at most one broker round-trip
// (avoiding the per-cluster rate limit). Concurrent callers serialize on ngMu and
// share the single fetch.
func (s *Server) cachedNodeGroups(ctx context.Context) (*brokerapi.NodeGroupListResponse, error) {
	s.ngMu.Lock()
	defer s.ngMu.Unlock()
	if s.ngCache != nil && time.Since(s.ngCacheAt) < nodeGroupsCacheTTL {
		return s.ngCache, nil
	}
	resp, err := s.agent.GetNodeGroups(ctx)
	if err != nil {
		return nil, err
	}
	s.ngCache = resp
	s.ngCacheAt = time.Now()
	return resp, nil
}

// New validates opts, builds the *tls.Config, and returns a Server
// ready to Run. It binds no socket and performs no network I/O.
func New(opts Options) (*Server, error) {
	if opts.BindAddress == "" {
		return nil, errors.New("grpcserver: BindAddress is required")
	}
	tlsCfg, err := opts.TLS.Build()
	if err != nil {
		return nil, fmt.Errorf("grpcserver: %w", err)
	}

	logger := opts.Logger
	if logger.GetSink() == nil {
		logger = log.Log.WithName("grpcserver")
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = DefaultShutdownTimeout
	}

	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	s := &Server{opts: opts, log: logger, agent: opts.AgentClient, grpcSrv: gs}
	protos.RegisterCloudProviderServer(gs, s)
	return s, nil
}

// Run binds the listener and blocks until ctx is cancelled. Returns
// nil on graceful shutdown, the underlying error otherwise.
func (s *Server) Run(ctx context.Context) error {
	l, err := net.Listen("tcp", s.opts.BindAddress)
	if err != nil {
		return fmt.Errorf("grpcserver: listen on %q: %w", s.opts.BindAddress, err)
	}
	s.lnMu.Lock()
	s.listener = l
	s.lnMu.Unlock()

	s.log.Info("starting externalgrpc server", "bindAddress", l.Addr().String())

	serveErr := make(chan error, 1)
	go func() {
		err := s.grpcSrv.Serve(l)
		// grpc.Serve returns nil on GracefulStop; anything else is a
		// real listener-side error worth surfacing.
		serveErr <- err
	}()

	select {
	case <-ctx.Done():
		s.log.Info("draining externalgrpc server", "timeout", s.opts.ShutdownTimeout)
		done := make(chan struct{})
		go func() {
			s.grpcSrv.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(s.opts.ShutdownTimeout):
			s.log.Info("graceful shutdown deadline exceeded; forcing stop")
			s.grpcSrv.Stop()
		}
		// When the stop wins the race against Serve starting, Serve returns
		// ErrServerStopped: that is still a clean shutdown, not a failure.
		if err := <-serveErr; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("grpcserver: serve: %w", err)
		}
		return nil
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("grpcserver: serve: %w", err)
		}
		return nil
	}
}

// Addr exposes the listener's resolved address; useful in tests that
// bind to ":0" and need to learn the OS-assigned port.
func (s *Server) Addr() net.Addr {
	s.lnMu.Lock()
	defer s.lnMu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}
