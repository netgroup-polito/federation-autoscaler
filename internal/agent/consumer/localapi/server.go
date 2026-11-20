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
//	GET    /local/nodegroups             proxy to client.GetNodeGroups, or built
//	                                     by the local brain in local mode
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
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/brain"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/latency"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/broker/chunk"
)

// Prober measures round-trip time to provider UDP echo endpoints for the
// measured-latency strategy. Satisfied by *latency.Prober; an interface so tests
// can inject a deterministic fake.
type Prober interface {
	MeasureAndPick(ctx context.Context, cands []latency.Candidate) latency.Result
}

// measurer is the optional part of a Prober that measures without picking
// (*latency.Prober has it). ConsumerChoice uses it to give the local AI the
// measured latency of the nearest providers.
type measurer interface {
	Measure(ctx context.Context, endpoints []string) map[string]float64
}

// aiMeasuredProviders is how many of the nearest providers are measured for
// the local AI, like the Latency policy's shortlist: latency mostly follows
// distance, so the farther ones are left unmeasured.
const aiMeasuredProviders = 3

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

	// AIJob is what the AI does for a plain-language request: ollama.JobRank
	// (the default when empty: the model ranks the providers) or
	// ollama.JobFields (the model only turns the text into fields and the
	// providers are filtered and ranked in code, see ollama.RankByIntent).
	AIJob string

	// Brain, when set, makes the placement decision locally (local mode):
	// GET /local/nodegroups is built from the catalog by the brain instead of
	// being fetched from the Broker. Booking still goes through the Broker.
	// Nil means central mode (the Broker decides).
	Brain *brain.Brain

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

	// aiMutex guards aiCache and lastSel. aiCache holds one ranking per
	// plain-language request text (the ConsumerPolicy's, or a ResourceRequest's
	// own), so requests with different texts never share a decision.
	aiMutex  sync.Mutex
	aiCache  map[string]*aiDecision
	lastSel  *brokerapi.ConsumerChoiceReport
	ollama   *ollama.Client
	location func() *ollama.Location
	aiJob    string

	brain *brain.Brain // nil in central mode
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
	case !ollama.ValidJob(opts.AIJob):
		return nil, fmt.Errorf("localapi: AIJob must be %q or %q, got %q", ollama.JobRank, ollama.JobFields, opts.AIJob)
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
		aiJob:    opts.AIJob,
		brain:    opts.Brain,
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
	// A ResourceRequest may carry its own latency bound, plain-language request
	// and size for this one call.
	q, err := DecodeNodeGroupQuery(r.URL.Query())
	if err != nil {
		s.writeRawError(w, http.StatusBadRequest, brokerapi.ErrCodeInvalidRequest, err.Error())
		return
	}
	resp, err := s.nodeGroups(r.Context(), q)
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

	// ConsumerChoice: the local AI ranks the providers for a plain-language
	// request (a ResourceRequest's own text, else the ConsumerPolicy's) and
	// only the first-ranked provider that can hold the request stays growable.
	// It mirrors the Broker's masking but runs client-side, so the Broker stays
	// unaware.
	if s.ollama != nil {
		prompt, perRequest := q.Prompt, q.Prompt != ""
		policyCC := false
		if !perRequest {
			policyCC, prompt = s.currentConsumerChoice(r.Context())
		}
		if perRequest || policyCC {
			s.serveConsumerChoice(w, resp, prompt, perRequest, q.Need, q.Bound)
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
		maskToFirstFitting(resp, ranked, q.Need) // nil ranking (no capacity) masks everything
	}

	s.serveNodeGroups(w, resp)
}

// aiCacheTTL is how long a ranking is reused before the model is asked again.
const aiCacheTTL = 60 * time.Second

// maxAICacheEntries bounds aiCache: past it, the oldest finished entries go.
const maxAICacheEntries = 32

// aiDecision is one cached ranking for one request text.
type aiDecision struct {
	ranked  []string
	at      time.Time
	source  string
	running bool
}

// serveConsumerChoice serves the view masked to the AI's choice for prompt.
// The ranking is cached per prompt for aiCacheTTL. While the model is still
// answering, every provider is masked, so the caller waits and asks again.
func (s *Server) serveConsumerChoice(
	w http.ResponseWriter, resp *brokerapi.NodeGroupListResponse,
	prompt string, perRequest bool, need corev1.ResourceList, bound *autoscalingv1alpha1.LatencyBound,
) {
	if resp.AppliedPlacement != autoscalingv1alpha1.PlacementStrategyConsumerChoice {
		if perRequest && s.brain == nil {
			// Central mode under another policy: the Broker already masked
			// the list for that policy, so there is nothing left to rank.
			// The ConsumerPolicy decides, as it would without the text.
			s.log.V(1).Info("per-request prompt not applied: in central mode it needs the ConsumerChoice policy",
				"brokerApplies", resp.AppliedPlacement)
			s.serveNodeGroups(w, resp)
			return
		}
		// The Broker learns a policy change on the next heartbeat (up to 15 s
		// later). Until then its list is still masked for the previous policy,
		// often down to one provider, and a choice made on it would just
		// repeat that pick and stay cached for a minute. Wait for the unmasked
		// ConsumerChoice list instead.
		s.log.V(1).Info("ConsumerChoice: waiting for the Broker to apply the policy",
			"brokerApplies", resp.AppliedPlacement)
		maskAll(resp)
		resp.Waiting = brokerapi.WaitingPolicyApplying
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	// Nothing can be grown at all (every provider full, or excluded by a
	// latency limit): there is nothing for the AI to rank, so do not ask it
	// (asking would only repeat on every poll) and do not say it is choosing.
	// The caller sees an honest "no capacity".
	if !anyGrowable(resp) {
		s.serveNodeGroups(w, resp)
		return
	}

	s.aiMutex.Lock()
	if s.aiCache == nil {
		s.aiCache = map[string]*aiDecision{}
	}
	d := s.aiCache[prompt]
	// A finished decision is reused for aiCacheTTL, even an empty one (no
	// provider could be ranked): re-asking every poll would loop.
	if d != nil && !d.running && !d.at.IsZero() && time.Since(d.at) < aiCacheTTL {
		ranked := d.ranked
		if perRequest && s.lastSel != nil && s.lastSel.Prompt == prompt {
			s.lastSel.Requested = requestedSize(need, bound)
		}
		s.aiMutex.Unlock()
		maskToFirstFitting(resp, ranked, need)
		s.serveNodeGroups(w, resp)
		return
	}
	if d == nil {
		d = &aiDecision{}
		s.aiCache[prompt] = d
		s.pruneAICacheLocked()
		// A new ConsumerPolicy text: the dashboard must not keep showing the
		// previous text's ranking while the new one is being made.
		if !perRequest && s.lastSel != nil && !s.lastSel.PerRequest {
			s.lastSel = nil
		}
	}
	if !d.running {
		d.running = true
		groups := make([]brokerapi.NodeGroupView, len(resp.NodeGroups))
		copy(groups, resp.NodeGroups)
		client := s.ollama
		if s.location != nil {
			if loc := s.location(); loc != nil {
				client = client.WithConsumerLocation(loc)
			}
		}
		go s.runSelection(client, prompt, perRequest, need, bound, groups)
	}
	s.aiMutex.Unlock()

	// While the model is answering, nothing is growable, so the caller (the
	// Cluster Autoscaler, or a ResourceRequest) waits for the next poll. The
	// Waiting note tells a ResourceRequest why, so it does not report "no
	// capacity".
	maskAll(resp)
	resp.Waiting = brokerapi.WaitingAIChoosing
	s.writeJSON(w, http.StatusOK, resp)
}

// runSelection asks the model to rank groups for prompt and caches the result
// (or the deterministic fallback when the model cannot be used).
func (s *Server) runSelection(
	client *ollama.Client, prompt string, perRequest bool, need corev1.ResourceList,
	bound *autoscalingv1alpha1.LatencyBound, groups []brokerapi.NodeGroupView,
) {
	// Longer than the client's own timeout, so the configured --ollama-timeout
	// is what bounds the call, never this context.
	ctx, cancel := context.WithTimeout(context.Background(), client.Timeout()+30*time.Second)
	defer cancel()

	s.log.V(1).Info("AI selection background task started", "prompt", prompt, "perRequest", perRequest, "job", s.job())
	rtts := s.measureNearest(ctx, groups)

	var (
		trace      *ollama.Trace
		aiErr      error
		source     string
		ranked     []string
		understood string
	)
	if s.job() == ollama.JobFields {
		// The AI only translates the text; the providers are filtered and
		// ranked in code. The ranking is not completed with the other
		// providers: a place, an exclusion or "renewable only" is a must, so
		// a request whose providers are full waits instead of going elsewhere.
		res := ollama.UnderstandIntent(ctx, client, prompt, ollama.KnownPlaces(groups))
		trace = res.Trace
		if res.AIError != "" {
			aiErr = errors.New(res.AIError)
		}
		source = SelectionSourceAI
		if res.Source != ollama.AmountSourceAI {
			source = SelectionSourceFallback
		}
		var loc *ollama.Location
		if s.location != nil {
			loc = s.location()
		}
		ranked = ollama.RankByIntent(groups, res.Intent, rtts, loc)
		understood = res.Intent.String()
	} else {
		if len(rtts) > 0 {
			client = client.WithMeasuredLatencies(rtts)
		}
		// "not in X" is applied in code before the model sees the list: a
		// small model ignores exclusions. Excluded providers are never
		// ranked, so they are never grown for this request.
		if excl := ollama.ParseIntent(prompt, ollama.KnownPlaces(groups)).Exclude; len(excl) > 0 {
			groups = ollama.WithoutPlaces(groups, excl)
			understood = "not in " + strings.Join(excl, ", ")
		}
		trace, aiErr = client.SelectDetailed(ctx, prompt, groups)
		source, ranked = SelectionSourceAI, trace.Ranked
		if aiErr != nil || len(ranked) == 0 {
			source = SelectionSourceFallback
			ranked, _ = ollama.DeterministicFallback(groups)
		}
		ranked = completeRanking(ranked, groups)
	}
	if trace == nil {
		trace = &ollama.Trace{}
	}
	now := time.Now()

	s.aiMutex.Lock()
	d := s.aiCache[prompt]
	if d == nil { // pruned while the model was answering
		d = &aiDecision{}
		s.aiCache[prompt] = d
	}
	d.ranked, d.at, d.source, d.running = ranked, now, source, false
	s.lastSel = &brokerapi.ConsumerChoiceReport{
		Prompt:     prompt,
		Ranked:     append([]string(nil), ranked...),
		Source:     source,
		DecidedAt:  metav1.NewTime(now),
		PerRequest: perRequest,
		Understood: understood,
	}
	if perRequest {
		s.lastSel.Requested = requestedSize(need, bound)
	}
	s.aiMutex.Unlock()

	// One Info line per decision: what the model was asked, what it answered
	// and what the agent will act on. It is the operator's record of every
	// ConsumerChoice decision.
	errMsg := ""
	if aiErr != nil {
		errMsg = aiErr.Error()
	}
	s.log.Info(SelectionFinishedMessage,
		"source", source,
		"prompt", prompt,
		"perRequest", perRequest,
		"ranked", ranked,
		"understood", understood,
		"rawResponse", trace.RawResponse,
		"errorKind", string(trace.ErrorKind),
		"error", errMsg,
		"durationMs", trace.Duration().Milliseconds())
}

// job is the AI's job for plain-language requests (JobRank when unset).
func (s *Server) job() string {
	if s.aiJob == ollama.JobFields {
		return ollama.JobFields
	}
	return ollama.JobRank
}

// requestedSize renders a request's size and latency limit for the
// dashboard, e.g. "4 CPU, 8Gi memory, max 3 ms"; empty when neither is given.
func requestedSize(need corev1.ResourceList, bound *autoscalingv1alpha1.LatencyBound) string {
	a := ollama.Amount{CPU: need[corev1.ResourceCPU], Memory: need[corev1.ResourceMemory]}
	if g, ok := need[chunk.GPUResourceName]; ok {
		a.GPU = g.Value()
	}
	if bound != nil {
		a.MaxLatencyMs = bound.MaxLatencyMs
	}
	if a.IsZero() {
		return ""
	}
	return a.String()
}

// measureNearest measures the round-trip time to the aiMeasuredProviders
// providers nearest to this consumer (by distance; in list order when the
// location is unknown) that have a probe endpoint, keyed by provider ID.
// Nil when there is no prober that can measure.
func (s *Server) measureNearest(ctx context.Context, groups []brokerapi.NodeGroupView) map[string]float64 {
	m, ok := s.prober.(measurer)
	if !ok || m == nil {
		return nil
	}
	type cand struct {
		id, endpoint string
		km           float64
	}
	var loc *ollama.Location
	if s.location != nil {
		loc = s.location()
	}
	seen := map[string]bool{}
	cands := make([]cand, 0, len(groups))
	for _, ng := range groups {
		if ng.ProbeEndpoint == "" || seen[ng.ProviderClusterID] || ng.MaxSize <= ng.CurrentReserved {
			continue
		}
		seen[ng.ProviderClusterID] = true
		c := cand{id: ng.ProviderClusterID, endpoint: ng.ProbeEndpoint}
		if loc != nil && ng.Topology != nil {
			c.km = brokerapi.HaversineKm(loc.Latitude, loc.Longitude, ng.Topology.Latitude, ng.Topology.Longitude)
		}
		cands = append(cands, c)
	}
	if loc != nil {
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].km < cands[j].km })
	}
	if len(cands) > aiMeasuredProviders {
		cands = cands[:aiMeasuredProviders]
	}
	if len(cands) == 0 {
		return nil
	}
	endpoints := make([]string, len(cands))
	for i, c := range cands {
		endpoints[i] = c.endpoint
	}
	byEndpoint := m.Measure(ctx, endpoints)
	out := map[string]float64{}
	for _, c := range cands {
		if v, ok := byEndpoint[c.endpoint]; ok && !math.IsInf(v, 0) && !math.IsNaN(v) {
			out[c.id] = v
		}
	}
	s.log.V(1).Info("measured the nearest providers for the AI", "rttsMs", out)
	return out
}

// pruneAICacheLocked drops the oldest finished entries once aiCache is over
// maxAICacheEntries. The caller holds aiMutex.
func (s *Server) pruneAICacheLocked() {
	for len(s.aiCache) > maxAICacheEntries {
		oldest := ""
		var oldestAt time.Time
		for k, d := range s.aiCache {
			if d.running {
				continue
			}
			if oldest == "" || d.at.Before(oldestAt) {
				oldest, oldestAt = k, d.at
			}
		}
		if oldest == "" {
			return // everything is running; let it finish
		}
		delete(s.aiCache, oldest)
	}
}

// anyGrowable reports whether any node group still has room.
func anyGrowable(resp *brokerapi.NodeGroupListResponse) bool {
	for _, ng := range resp.NodeGroups {
		if ng.MaxSize > ng.CurrentReserved {
			return true
		}
	}
	return false
}

// maskAll leaves no provider growable.
func maskAll(resp *brokerapi.NodeGroupListResponse) {
	for i := range resp.NodeGroups {
		resp.NodeGroups[i].MaxSize = resp.NodeGroups[i].CurrentReserved
	}
}

// LastSelection returns the latest ConsumerChoice decision for the heartbeat
// (the dashboard's "AI request → ranking"): the prompt, the ranking the agent
// acts on, whether the model or the fallback made it, and whether it was for a
// ResourceRequest's own text. Nil before the first decision, and while a new
// ConsumerPolicy text is being decided. It stays reported after its cache
// expires: it is still the last decision made.
func (s *Server) LastSelection() *brokerapi.ConsumerChoiceReport {
	s.aiMutex.Lock()
	defer s.aiMutex.Unlock()
	if s.lastSel == nil {
		return nil
	}
	cp := *s.lastSel
	cp.Ranked = append([]string(nil), s.lastSel.Ranked...)
	return &cp
}

// serveNodeGroups writes the final node-group view. In local mode it first
// records the brain's pick for the heartbeat (the dashboard's "decided
// locally"). The temporary all-masked view served while the LLM is still
// answering is written directly instead, so it never shows up as a choice.
func (s *Server) serveNodeGroups(w http.ResponseWriter, resp *brokerapi.NodeGroupListResponse) {
	if s.brain != nil {
		s.brain.RecordChoice(resp)
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// nodeGroups returns the node-group view to serve: built by the local brain in
// local mode, fetched from the Broker in central mode. Everything after it
// (latency re-mask, ConsumerChoice) is the same in both modes. q carries a
// ResourceRequest's own latency bound and text; only the local brain applies
// them here (the central Broker supports neither). A per-request text makes the
// brain return every provider unmasked, for the AI to rank.
func (s *Server) nodeGroups(ctx context.Context, q NodeGroupQuery) (*brokerapi.NodeGroupListResponse, error) {
	if s.brain != nil {
		var placement *autoscalingv1alpha1.PlacementPolicy
		if q.Prompt != "" && s.ollama != nil {
			placement = &autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyConsumerChoice}
		}
		return s.brain.NodeGroupsFor(ctx, q.Bound, placement)
	}
	if q.Bound != nil {
		s.log.V(1).Info("latency bound ignored: the central Broker does not apply bounds (use --brain=local)",
			"maxLatencyMs", q.Bound.MaxLatencyMs)
	}
	return s.client.GetNodeGroups(ctx)
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
	SelectionFinishedMessage = "ConsumerChoice selection finished"
	// SelectionSourceAI: the ranking is the model's. SelectionSourceFallback:
	// the model could not be used (unreachable, timeout, invalid answer) and the
	// deterministic ranking was used instead.
	SelectionSourceAI       = brokerapi.ChoiceSourceAI
	SelectionSourceFallback = brokerapi.ChoiceSourceFallback
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

// maskToFirstFitting keeps growable only the first provider in ranked order
// that can hold need, masking every other provider. A provider fits when one of
// its node groups has the right chunk type (GPU or not) and enough free chunks
// for need; an empty need fits any free capacity. When nobody fits, everything
// is masked (the caller waits).
func maskToFirstFitting(resp *brokerapi.NodeGroupListResponse, ranked []string, need corev1.ResourceList) {
	fits := map[string]bool{}
	wantGPU := chunk.WantsGPU(need)
	for i := range resp.NodeGroups {
		ng := &resp.NodeGroups[i]
		free := ng.MaxSize - ng.CurrentReserved
		if free <= 0 {
			continue
		}
		if len(need) == 0 {
			fits[ng.ProviderClusterID] = true
			continue
		}
		if wantGPU != (ng.Type == brokerv1alpha1.ChunkTypeGPU) {
			continue
		}
		if n := chunk.CountFor(need, ng.ChunkResources); n > 0 && n <= free {
			fits[ng.ProviderClusterID] = true
		}
	}
	chosen := ""
	for _, id := range ranked {
		if fits[id] {
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
		if s.brain != nil {
			// The brain chose on a catalog that can be a few seconds old; a
			// "no free chunk" refusal makes its next decision move on.
			s.brain.ObserveReservation(req.ProviderClusterID, err)
		}
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
