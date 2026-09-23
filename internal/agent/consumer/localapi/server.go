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

// Package localapi is the consumer-side loopback REST server
// (docs/design.md §4.3). It runs on 127.0.0.1 by default and is
// consumed by the co-located gRPC server (step 10) — every Cluster
// Autoscaler RPC that needs broker-side state flows through this
// surface so the gRPC server never holds Broker mTLS credentials.
//
// Routes:
//
//	GET    /local/nodegroups             proxy to client.GetNodeGroups
//	POST   /local/reservations           proxy to client.PostReservation
//	DELETE /local/reservations/{id}      proxy to client.DeleteReservation
//	GET    /local/virtual-nodes          list VirtualNodeState CRs in the
//	                                     agent's namespace, projected to
//	                                     the wire shape the gRPC server
//	                                     consumes
//
// Failures from the broker are forwarded with the broker's HTTP status
// and ErrorResponse body so the gRPC server can map them to gRPC codes
// without losing context.
package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/latency"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// Prober measures round-trip time to provider UDP echo endpoints for the
// measured-latency strategy. Satisfied by *latency.Prober; an interface so tests
// can inject a deterministic fake.
type Prober interface {
	MeasureAndPick(ctx context.Context, cands []latency.Candidate) latency.Result
}

// Options bundles the construction-time settings of the loopback REST
// server.
type Options struct {
	// BindAddress is the listener address. Production wires
	// "127.0.0.1:9090" from --local-api-bind-address; binding to a
	// non-loopback host is allowed but logged as a warning.
	BindAddress string

	// Client is the Broker HTTP client. Every proxy route flows through
	// it. Required.
	Client *agentclient.Client

	// LocalClient reads the consumer-cluster's VirtualNodeState CRs for
	// /local/virtual-nodes. Required.
	LocalClient ctrlclient.Client

	// Namespace scopes the VirtualNodeState list served by
	// /local/virtual-nodes. Empty means "all namespaces"; production
	// wires the same namespace the Peer / Unpeer handlers write CRs into
	// so the loopback view stays consistent with the agent's own writes.
	Namespace string

	// Prober measures RTT to provider echo endpoints for the measured-latency
	// strategy. When set, GET /local/nodegroups re-masks a latency shortlist to
	// the lowest-RTT provider before returning it to CA. Nil disables probing
	// (the broker's shortlist is passed through unchanged).
	Prober Prober

	// Logger is the structured logger every handler logs through.
	// Defaults to controller-runtime's logger named "consumer-localapi".
	Logger logr.Logger

	// OllamaClient is the optional AI client for the ConsumerChoice strategy.
	// When non-nil AND the current ConsumerPolicy is ConsumerChoice, the
	// handleNodeGroups handler delegates provider selection to the LLM.
	// Nil disables AI-driven selection.
	OllamaClient *ollama.Client

	// ConsumerLocation returns where this consumer is (the location its
	// heartbeat reports), or nil while unknown. It is sent to the LLM with the
	// provider list so that a request like "close to me" has a referent. Nil
	// func: no location is sent.
	ConsumerLocation func() *ollama.Location

	// ShutdownTimeout caps how long Run waits for in-flight requests
	// to drain when ctx is cancelled. Defaults to 5 s.
	ShutdownTimeout time.Duration
}

// Server is the local-API HTTP listener.
type Server struct {
	bind     string
	client   *agentclient.Client
	local    ctrlclient.Client
	ns       string
	prober   Prober
	log      logr.Logger
	shutdown time.Duration
	srv      *http.Server

	aiMutex     sync.Mutex
	aiRunning   bool
	aiResult    []string
	aiTimestamp time.Time
	aiPrompt    string
	ollama      *ollama.Client
	location    func() *ollama.Location
}

// New validates opts and returns a Server ready to Run. It performs no
// network I/O.
func New(opts Options) (*Server, error) {
	switch {
	case opts.BindAddress == "":
		return nil, errors.New("localapi: BindAddress is required")
	case opts.Client == nil:
		return nil, errors.New("localapi: Client is required")
	case opts.LocalClient == nil:
		return nil, errors.New("localapi: LocalClient is required")
	}
	logger := opts.Logger
	if logger.GetSink() == nil {
		logger = log.Log.WithName("consumer-localapi")
	}
	shutdown := opts.ShutdownTimeout
	if shutdown <= 0 {
		shutdown = 5 * time.Second
	}
	s := &Server{
		bind:     opts.BindAddress,
		client:   opts.Client,
		local:    opts.LocalClient,
		ns:       opts.Namespace,
		prober:   opts.Prober,
		log:      logger,
		shutdown: shutdown,
		ollama:   opts.OllamaClient,
		location: opts.ConsumerLocation,
	}
	s.srv = &http.Server{
		Addr:              opts.BindAddress,
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

// Handler returns the bare router. Exposed so tests can mount the
// server on an httptest.NewServer without binding a real socket.
func (s *Server) Handler() http.Handler { return s.handler() }

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /local/nodegroups", s.handleNodeGroups)
	mux.HandleFunc("POST /local/reservations", s.handleReservationCreate)
	mux.HandleFunc("DELETE /local/reservations/{id}", s.handleReservationDelete)
	mux.HandleFunc("GET /local/virtual-nodes", s.handleVirtualNodes)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

// Run binds the listener and blocks until ctx is cancelled. Returns nil
// on graceful shutdown, the underlying error otherwise.
func (s *Server) Run(ctx context.Context) error {
	if !strings.HasPrefix(s.bind, "127.0.0.1:") && !strings.HasPrefix(s.bind, "[::1]:") {
		s.log.Info("WARNING: binding loopback REST server to a non-loopback address",
			"bindAddress", s.bind)
	}
	s.log.Info("starting consumer loopback REST server", "bindAddress", s.bind)

	shutdownDone := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdown)
		defer cancel()
		shutdownDone <- s.srv.Shutdown(shutdownCtx)
	}()

	err := s.srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return <-shutdownDone
	}
	return err
}

// -----------------------------------------------------------------------------
// Handlers
// -----------------------------------------------------------------------------

func (s *Server) handleNodeGroups(w http.ResponseWriter, r *http.Request) {
	resp, err := s.client.GetNodeGroups(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	// Measured-latency strategy: the Broker exposed the nearest-by-distance
	// shortlist; probe them and narrow to the lowest-RTT provider so CA grows
	// exactly it. Covers both CA's read and NodeGroupIncreaseSize's re-fetch
	// (both hit this handler). No prober / not a shortlist ⇒ pass through.
	if resp.LatencyShortlist && s.prober != nil {
		s.maskToMeasuredWinner(r.Context(), resp)
	}

	// ConsumerChoice interception: when the active policy is ConsumerChoice and
	// an Ollama client is configured, delegate provider selection to the local
	// LLM. This mirrors the Broker's masking (MaxSize = CurrentReserved for all
	// but the chosen provider) but runs client-side so the Broker stays unaware.
	if s.ollama != nil {
		if policy, prompt := s.currentConsumerChoice(r.Context()); policy {
			s.aiMutex.Lock()

			// If the prompt changed, clear cache
			if s.aiPrompt != prompt {
				s.aiResult = nil
				s.aiPrompt = prompt
				s.aiTimestamp = time.Time{}
			}

			// If cache is fresh (< 60s), use it
			if len(s.aiResult) > 0 && time.Since(s.aiTimestamp) < 60*time.Second {
				ranked := s.aiResult
				s.aiMutex.Unlock()

				maskToFirstAvailable(resp, ranked)
				s.writeJSON(w, http.StatusOK, resp)
				return
			}

			// If not running, start it
			if !s.aiRunning {
				s.aiRunning = true

				nodeGroupsCopy := make([]brokerapi.NodeGroupView, len(resp.NodeGroups))
				copy(nodeGroupsCopy, resp.NodeGroups)

				client := s.ollama
				if s.location != nil {
					if loc := s.location(); loc != nil {
						client = client.WithConsumerLocation(loc)
					}
				}

				go func(prompt string, groups []brokerapi.NodeGroupView) {
					// Longer than the client's own timeout, so the configured
					// --ollama-timeout is what bounds the call, never this context.
					ctx, cancel := context.WithTimeout(context.Background(), client.Timeout()+30*time.Second)
					defer cancel()

					s.log.V(1).Info("AI selection background task started", "prompt", prompt)
					trace, aiErr := client.SelectDetailed(ctx, prompt, groups)

					s.aiMutex.Lock()
					defer s.aiMutex.Unlock()
					s.aiRunning = false

					// The prompt changed while the model was answering the old one:
					// that ranking answers a request nobody is making any more. Drop
					// it; the next poll starts a selection for the current prompt.
					if s.aiPrompt != prompt {
						s.log.Info(SelectionDiscardedMessage, "prompt", prompt)
						return
					}

					source, ranked := SelectionSourceAI, trace.Ranked
					if aiErr != nil || len(ranked) == 0 {
						source = SelectionSourceFallback
						ranked, _ = ollama.DeterministicFallback(groups)
					}
					s.aiResult = completeRanking(ranked, groups)
					s.aiTimestamp = time.Now()

					// One Info line per decision: what the model was asked, what it
					// answered and what the agent will act on. It is the operator's
					// record of every ConsumerChoice decision.
					errMsg := ""
					if aiErr != nil {
						errMsg = aiErr.Error()
					}
					s.log.Info(SelectionFinishedMessage,
						"source", source,
						"prompt", prompt,
						"ranked", s.aiResult,
						"rawResponse", trace.RawResponse,
						"errorKind", string(trace.ErrorKind),
						"error", errMsg,
						"durationMs", trace.Duration().Milliseconds())
				}(prompt, nodeGroupsCopy)
			}
			s.aiMutex.Unlock()

			// While AI is running, we return no available capacity
			// so the Autoscaler waits for the next poll.
			for i := range resp.NodeGroups {
				resp.NodeGroups[i].MaxSize = resp.NodeGroups[i].CurrentReserved
			}
			s.writeJSON(w, http.StatusOK, resp)
			return
		}
	}

	// ConsumerChoice without an LLM decision: no --ollama-url, or the local
	// ConsumerPolicy could not be read or has already moved on while the Broker
	// (up to a heartbeat behind) still applies ConsumerChoice. The Broker hands
	// that list over unmasked -- the choice is the consumer's -- so passing it
	// through would let the Cluster Autoscaler grow every provider at once. Do
	// what --ollama-url's help promises instead and pick with the deterministic
	// ranking the LLM path falls back to. AppliedPlacement comes from the
	// Broker's own response, so this costs no extra ConsumerPolicy read.
	if resp.AppliedPlacement == autoscalingv1alpha1.PlacementStrategyConsumerChoice {
		ranked, _ := ollama.DeterministicFallback(resp.NodeGroups)
		maskToFirstAvailable(resp, ranked) // nil ranking (no capacity) masks everything
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// maskToMeasuredWinner probes the growable, probeable providers in a latency
// shortlist and masks every other growable entry (MaxSize = CurrentReserved) so
// only the lowest-RTT provider stays growable. It mutates resp in place. If no
// candidate is probeable, or none answers, it leaves the Broker's shortlist as-is
// (safe degrade to distance-based selection).
func (s *Server) maskToMeasuredWinner(ctx context.Context, resp *brokerapi.NodeGroupListResponse) {
	seen := map[string]bool{}
	var cands []latency.Candidate
	for i := range resp.NodeGroups {
		ng := &resp.NodeGroups[i]
		if ng.MaxSize-ng.CurrentReserved > 0 && ng.ProbeEndpoint != "" && !seen[ng.ProviderClusterID] {
			seen[ng.ProviderClusterID] = true
			cands = append(cands, latency.Candidate{
				ProviderClusterID: ng.ProviderClusterID,
				Endpoint:          ng.ProbeEndpoint,
			})
		}
	}
	if len(cands) == 0 {
		return // nothing measurable → keep the Broker's distance shortlist
	}
	res := s.prober.MeasureAndPick(ctx, cands)
	if res.Chosen == "" {
		return // every candidate unreachable → don't interfere
	}
	for i := range resp.NodeGroups {
		ng := &resp.NodeGroups[i]
		if ng.MaxSize-ng.CurrentReserved > 0 && ng.ProviderClusterID != res.Chosen {
			ng.MaxSize = ng.CurrentReserved
		}
	}
	s.log.V(1).Info("measured-latency: masked shortlist to lowest-RTT provider",
		"chosen", res.Chosen, "rtts", res.RTTs)
}

// Log messages of the ConsumerChoice decision. Stable on purpose: operators grep
// for them, and the ConsumerChoice end-to-end suite reads them to learn what the
// agent's own LLM call decided.
const (
	SelectionFinishedMessage  = "ConsumerChoice selection finished"
	SelectionDiscardedMessage = "ConsumerChoice selection discarded: prompt changed"
	// SelectionSourceAI: the ranking is the model's. SelectionSourceFallback:
	// the model could not be used (unreachable, timeout, invalid answer) and the
	// deterministic ranking was used instead.
	SelectionSourceAI       = "ai"
	SelectionSourceFallback = "fallback"
)

// completeRanking appends, in DeterministicFallback order, every provider with
// capacity that ranked does not mention. A model may rank only some of the
// candidates; once those fill up, maskToFirstAvailable would otherwise mask
// everything and stall the Cluster Autoscaler until the cached ranking expires.
func completeRanking(ranked []string, groups []brokerapi.NodeGroupView) []string {
	seen := make(map[string]bool, len(ranked))
	out := append([]string(nil), ranked...)
	for _, id := range ranked {
		seen[id] = true
	}
	fallback, _ := ollama.DeterministicFallback(groups)
	for _, id := range fallback {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// maskToFirstAvailable iterates the ranked provider list in order and picks the
// first provider that has available capacity (MaxSize > CurrentReserved),
// masking all others. If no provider in the list has capacity, everything is
// masked (CA waits).
func maskToFirstAvailable(resp *brokerapi.NodeGroupListResponse, ranked []string) {
	chosen := ""
	capacity := map[string]bool{}
	for i := range resp.NodeGroups {
		ng := &resp.NodeGroups[i]
		if ng.MaxSize-ng.CurrentReserved > 0 {
			capacity[ng.ProviderClusterID] = true
		}
	}
	for _, id := range ranked {
		if capacity[id] {
			chosen = id
			break
		}
	}
	for i := range resp.NodeGroups {
		if resp.NodeGroups[i].ProviderClusterID != chosen {
			resp.NodeGroups[i].MaxSize = resp.NodeGroups[i].CurrentReserved
		}
	}
}

// currentConsumerChoice reads the ConsumerPolicy CRD and returns (true, userPrompt)
// when the active placement strategy is ConsumerChoice, or (false, "") otherwise.
// Best-effort: a read error returns (false, "").
func (s *Server) currentConsumerChoice(ctx context.Context) (bool, string) {
	var list autoscalingv1alpha1.ConsumerPolicyList
	if err := s.local.List(ctx, &list, ctrlclient.InNamespace(s.ns)); err != nil {
		s.log.V(1).Info("read ConsumerPolicy for ConsumerChoice check failed", "err", err.Error())
		return false, ""
	}
	if len(list.Items) == 0 {
		return false, ""
	}
	// Pick the lowest-named policy (same rule as heartbeat.go).
	chosen := &list.Items[0]
	for i := range list.Items {
		if list.Items[i].Name < chosen.Name {
			chosen = &list.Items[i]
		}
	}
	if chosen.Spec.Placement.Type != autoscalingv1alpha1.PlacementStrategyConsumerChoice {
		return false, ""
	}
	return true, chosen.Spec.UserPrompt
}

func (s *Server) handleReservationCreate(w http.ResponseWriter, r *http.Request) {
	var req brokerapi.ReservationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeRawError(w, http.StatusBadRequest, brokerapi.ErrCodeInvalidRequest,
			fmt.Sprintf("decode body: %v", err))
		return
	}

	// Propagate the caller's idempotency key when present; mint a UUID
	// when absent so the broker still benefits from CRD-backed dedup.
	reservationID := r.Header.Get(brokerapi.HeaderReservationID)
	if reservationID == "" {
		reservationID = "res-" + uuid.NewString()
	}

	resp, err := s.client.PostReservation(r.Context(), reservationID, &req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set(brokerapi.HeaderReservationID, reservationID)
	s.writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleReservationDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.writeRawError(w, http.StatusBadRequest, brokerapi.ErrCodeInvalidRequest,
			"reservation id is required")
		return
	}
	resp, err := s.client.DeleteReservation(r.Context(), id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// handleVirtualNodes lists VirtualNodeState CRs on the consumer cluster
// and projects them into the local-API VirtualNodeView shape. The
// VirtualNodeStateReconciler is what keeps these CRs in sync with the
// underlying Liqo VirtualNode; this endpoint is the read-only surface
// the gRPC server consumes when answering Cluster Autoscaler's
// NodeGroupTargetSize / NodeGroupNodes / NodeGroupForNode RPCs.
//
// Entries are emitted for *every* VirtualNodeState regardless of phase
// so CA can observe in-flight scale-ups via NodeGroupTargetSize before
// Liqo finishes materialising the underlying node. The Name field
// prefers Status.VirtualNodeName (== the eventual v1.Node name) so
// NodeGroupForNode matches once the node is real; while the chunk is
// still Creating the CR name is surfaced as a stable placeholder.
func (s *Server) handleVirtualNodes(w http.ResponseWriter, r *http.Request) {
	listOpts := []ctrlclient.ListOption{}
	if s.ns != "" {
		listOpts = append(listOpts, ctrlclient.InNamespace(s.ns))
	}
	var list autoscalingv1alpha1.VirtualNodeStateList
	if err := s.local.List(r.Context(), &list, listOpts...); err != nil {
		s.log.Error(err, "list VirtualNodeState failed")
		s.writeRawError(w, http.StatusInternalServerError,
			brokerapi.ErrCodeInternalError, "list VirtualNodeState: "+err.Error())
		return
	}
	views := make([]VirtualNodeView, 0, len(list.Items))
	for i := range list.Items {
		views = append(views, virtualNodeStateToView(&list.Items[i]))
	}
	s.writeJSON(w, http.StatusOK, VirtualNodeListResponse{VirtualNodes: views})
}

// virtualNodeStateToView translates a CR into its wire representation.
// Name falls back to the CR's own name when Status.VirtualNodeName is
// empty (chunk still in Creating); see handleVirtualNodes for why this
// placeholder is intentional rather than an omitted entry.
func virtualNodeStateToView(vns *autoscalingv1alpha1.VirtualNodeState) VirtualNodeView {
	name := vns.Status.VirtualNodeName
	if name == "" {
		name = vns.Name
	}
	return VirtualNodeView{
		Name:                  name,
		VirtualNodeName:       vns.Status.VirtualNodeName,
		ProviderID:            vns.Status.ProviderID,
		ReservationID:         vns.Spec.ReservationID,
		NodeGroupID:           vns.Spec.NodeGroupID,
		ProviderClusterID:     vns.Spec.ProviderClusterID,
		ProviderLiqoClusterID: vns.Spec.ProviderLiqoClusterID,
		Phase:                 vns.Status.Phase,
		Allocatable:           vns.Status.Allocatable,
		LastTransitionTime:    vns.Status.LastTransitionTime,
	}
}

// -----------------------------------------------------------------------------
// Response helpers
// -----------------------------------------------------------------------------

func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.V(1).Info("encode response failed", "err", err.Error())
	}
}

// writeError forwards broker-originated errors to the caller with the
// broker's status code preserved, so the gRPC server in step 10 can
// map e.g. 412 PreconditionFailed → FAILED_PRECONDITION without
// re-inspecting strings.
func (s *Server) writeError(w http.ResponseWriter, err error) {
	var ce *agentclient.Error
	if errors.As(err, &ce) && ce.Status > 0 {
		s.writeJSON(w, ce.Status, brokerapi.ErrorResponse{
			Code:      ce.Code,
			Message:   ce.Message,
			RequestID: ce.RequestID,
		})
		return
	}
	s.writeRawError(w, http.StatusInternalServerError, brokerapi.ErrCodeInternalError, err.Error())
}

func (s *Server) writeRawError(w http.ResponseWriter, status int, code brokerapi.ErrorCode, msg string) {
	s.writeJSON(w, status, brokerapi.ErrorResponse{Code: code, Message: msg})
}
