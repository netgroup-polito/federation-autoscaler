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
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/netgroup-polito/federation-autoscaler/internal/broker/chunk"
)

// Server is the Broker's REST surface (docs/design.md §7.3). It wires every
// endpoint to a handler — advertisement endpoints (§7.3.1, §7.3.2) are
// implemented; remaining endpoints are stubs returning 501 Not Implemented
// until steps 4g/4h.
//
// Server is intentionally cheap to construct: NewServer does no I/O, takes no
// kubeconfig, and does not bind a TCP socket. The cmd/broker binary wraps it
// in a controller-runtime Runnable in step 4e.
type Server struct {
	log       logr.Logger
	client    client.Client
	namespace string
	sizer     chunk.Sizer
	consumers *ConsumerRegistry
}

// ServerOptions bundles the construction-time settings of a Server. Empty
// values fall back to package-level defaults; required values cause NewServer
// to panic on misconfiguration (matches the manager-time wiring style).
type ServerOptions struct {
	// Logger is the structured logger every handler logs through. Defaults
	// to the controller-runtime logger named "broker-api".
	Logger logr.Logger

	// Client is the controller-runtime client used by handlers to read and
	// write the four Broker-owned CRDs. The cache MUST already include
	// ClusterAdvertisement / Reservation / *Instruction (registered by the
	// reconcilers in cmd/broker/main.go).
	Client client.Client

	// Namespace is where every Broker-owned CR lives. Required.
	Namespace string

	// Sizer translates an advertised resource list into chunks. Defaults to
	// chunk.DefaultSizer (hard-coded design fallbacks).
	Sizer chunk.Sizer
}

// NewServer returns a Server ready to serve traffic via Handler(). Panics if
// Client or Namespace are missing — both are mandatory and the absence of
// either is a programmer error in cmd/broker, not a runtime condition.
func NewServer(opts ServerOptions) *Server {
	log := opts.Logger
	if log.GetSink() == nil {
		log = ctrl.Log.WithName("broker-api")
	}
	if opts.Client == nil {
		panic("broker api: ServerOptions.Client is required")
	}
	if opts.Namespace == "" {
		panic("broker api: ServerOptions.Namespace is required")
	}
	sizer := opts.Sizer
	if sizer == nil {
		sizer = chunk.NewDefaultSizer()
	}
	return &Server{
		log:       log,
		client:    opts.Client,
		namespace: opts.Namespace,
		sizer:     sizer,
		consumers: NewConsumerRegistry(),
	}
}

// Handler returns the http.Handler that owns every endpoint defined in
// docs/design.md §7.3 plus a generic /healthz probe. Built on Go 1.22+'s
// pattern-aware ServeMux so each route declares its own method.
//
// The middleware chain (request logging, panic recovery, request-ID
// propagation, per-cluster rate limiting) is layered on top of this Handler
// in step 4d; this method returns the bare mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Liveness probe — not part of §7.3 but useful for in-cluster health checks.
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// 7.3.1 — POST /api/v1/advertisements (Provider Agent, every 30 s)
	mux.HandleFunc("POST /api/v1/advertisements", s.handleAdvertisementUpsert)

	// 7.3.2 — GET /api/v1/advertisements/{clusterId} (Provider Agent)
	mux.HandleFunc("GET /api/v1/advertisements/{clusterId}", s.handleAdvertisementGet)

	// 7.3.3 — POST /api/v1/heartbeat (Consumer Agent, every 15 s)
	mux.HandleFunc("POST /api/v1/heartbeat", s.handleHeartbeat)

	// 7.3.4 — GET /api/v1/nodegroups (Consumer Agent)
	mux.HandleFunc("GET /api/v1/nodegroups", s.handleNodeGroupsList)

	// 7.3.5 — POST /api/v1/reservations (Consumer Agent, synchronous)
	mux.HandleFunc("POST /api/v1/reservations", s.handleReservationCreate)

	// 7.3.6 — GET /api/v1/instructions (Both agents, every 5 s)
	mux.HandleFunc("GET /api/v1/instructions", s.handleInstructionsList)

	// 7.3.7 — POST /api/v1/instructions/{id}/result (Both agents)
	mux.HandleFunc("POST /api/v1/instructions/{id}/result", s.handleInstructionResult)

	// 7.3.8 — GET /api/v1/reservations/{id} (Both agents)
	mux.HandleFunc("GET /api/v1/reservations/{id}", s.handleReservationGet)

	// 7.3.9 — DELETE /api/v1/reservations/{id} (Consumer Agent)
	mux.HandleFunc("DELETE /api/v1/reservations/{id}", s.handleReservationRelease)

	return mux
}

// -----------------------------------------------------------------------------
// Stub handlers — real implementations land in steps 4f–4h.
// -----------------------------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Real handlers:
//   - handleAdvertisementUpsert / handleAdvertisementGet — advertisement.go
//   - handleHeartbeat                                    — heartbeat.go
//   - handleReservationCreate / Get / Release            — reservation.go
//   - handleInstructionsList / handleInstructionResult   — instructions.go
//
// Last remaining stub (GET /api/v1/nodegroups) is left for a later step;
// the consumer flow does not block on it as long as the gRPC server can
// derive node groups from the advertisement-side caches.

func (s *Server) handleNodeGroupsList(w http.ResponseWriter, r *http.Request) {
	s.notImplemented(w, r, "GET /api/v1/nodegroups")
}

// -----------------------------------------------------------------------------
// Response helpers
// -----------------------------------------------------------------------------

// notImplemented returns the standard 501 ErrorResponse and echoes the
// X-Request-Id header in both the response and the structured log line.
func (s *Server) notImplemented(w http.ResponseWriter, r *http.Request, route string) {
	requestID := r.Header.Get(HeaderRequestID)
	s.log.V(1).Info("stub handler hit", "route", route, "requestId", requestID)
	writeError(w, http.StatusNotImplemented, ErrorResponse{
		Code:      ErrCodeInternalError,
		Message:   "endpoint not implemented yet",
		Details:   map[string]any{"route": route},
		RequestID: requestID,
	})
}

// writeJSON writes a 2xx JSON response with the canonical content type. It
// hides marshal failures behind a generic 500 to avoid leaking internal
// details to the agent.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Best-effort — the status line and content-type are already on the
		// wire so we cannot recover gracefully.
		_, _ = fmt.Fprintf(w, "\n")
	}
}

// writeError writes a 4xx/5xx JSON ErrorResponse. The HTTP status passed in
// MUST match the semantic of body.Code; the helper does not cross-check.
func writeError(w http.ResponseWriter, status int, body ErrorResponse) {
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
