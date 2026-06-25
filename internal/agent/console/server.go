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

// Package console is the agent-side config dashboard: a small, role-aware,
// plain-HTTP web UI that lets an operator drive a cluster's federation settings
// from a browser instead of `kubectl apply`-ing YAML by hand.
//
// It mirrors the broker's read-only dashboard pattern (a separate plain-HTTP
// listener, embedded single-page HTML, no auth) but — unlike that one — it
// WRITES cluster state through the agent's existing controller-runtime client:
//
//	consumer role:
//	  POST /api/policy    create/update/delete the `default` ConsumerPolicy
//	  POST /api/region    upsert the agent-location ConfigMap
//	  POST /api/workload  apply/delete the federation-demo burst workload
//	provider role:
//	  POST /api/prices    upsert the agent-prices ConfigMap
//	  POST /api/region    upsert the agent-location ConfigMap
//	  POST /api/capacity  upsert the agent-capacity ConfigMap
//	both roles:
//	  GET  /              the role's embedded single-page UI
//	  GET  /api/state     current values (so the UI can pre-select/pre-fill)
//	  GET  /api/regions   the selectable region list (internal/regions)
//	  GET  /healthz       liveness probe
//
// SECURITY: this listener is unauthenticated and, when exposed on a NodePort,
// lets anyone with node network reach mutate cluster state. It is demo-grade:
// the bind address defaults to empty (disabled) and overlays opt in. It must
// NOT be confused with, or merged into, the consumer's loopback localapi server
// (internal/agent/consumer/localapi), which is the gRPC server's private trust
// boundary and stays loopback-only.
package console

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/regions"
)

// Role discriminates which UI and which write endpoints the console exposes.
const (
	RoleConsumer = "consumer"
	RoleProvider = "provider"
)

// Resource coordinates the console reads from / writes to. The ConfigMap names
// and keys MUST match what the agent's loaders read (loadRegion / loadUnitPrices
// / loadCapacityPercents) and what the per-role kustomize overlays mount.
const (
	locationConfigMap = "agent-location"
	locationKey       = "location.yaml"
	pricesConfigMap   = "agent-prices"
	pricesKey         = "prices.yaml"
	capacityConfigMap = "agent-capacity"
	capacityKey       = "capacity.yaml"

	// The burst workload lives in `default` (offloaded LocalAndRemote), NOT in
	// the agent namespace — the consumer overlay's scoped Role grants the write.
	workloadNamespace = "default"
	workloadName      = "federation-demo"

	// consumerPolicyName is the single ConsumerPolicy the console manages. A
	// hand-created sibling CR would still influence placement; the console only
	// owns this one (the heartbeat picks the lowest-named CR).
	consumerPolicyName = "default"
)

//go:embed assets/consumer.html
var consumerHTML []byte

//go:embed assets/provider.html
var providerHTML []byte

// Options bundles the construction-time settings of the console server.
type Options struct {
	// Role is "consumer" or "provider"; it selects the UI and the write
	// endpoints. Required.
	Role string

	// BindAddress is the plain-HTTP listener address (e.g. ":9095"). The caller
	// MUST skip construction when this is empty (console disabled). Required.
	BindAddress string

	// LocalClient is the agent's writable controller-runtime client for the
	// local cluster — the same one used by the instruction handlers. Required.
	LocalClient ctrlclient.Client

	// Namespace is where the ConsumerPolicy and the agent-* ConfigMaps live
	// (the agent's own namespace). The workload Deployment is always written to
	// `default`, independent of this. Defaults to federation-autoscaler-system.
	Namespace string

	// Logger is the structured logger every handler logs through. Defaults to
	// controller-runtime's logger named "console".
	Logger logr.Logger

	// ShutdownTimeout caps how long Run waits for in-flight requests to drain on
	// ctx cancellation. Defaults to 5 s.
	ShutdownTimeout time.Duration
}

// Server is the console HTTP listener.
type Server struct {
	role     string
	bind     string
	local    ctrlclient.Client
	ns       string
	log      logr.Logger
	shutdown time.Duration

	srv *http.Server
}

// New validates opts and returns a Server ready to Run. It performs no network
// I/O.
func New(opts Options) (*Server, error) {
	switch {
	case opts.Role != RoleConsumer && opts.Role != RoleProvider:
		return nil, fmt.Errorf("console: Role must be %q or %q, got %q",
			RoleConsumer, RoleProvider, opts.Role)
	case opts.BindAddress == "":
		return nil, errors.New("console: BindAddress is required")
	case opts.LocalClient == nil:
		return nil, errors.New("console: LocalClient is required")
	}
	logger := opts.Logger
	if logger.GetSink() == nil {
		logger = log.Log.WithName("console")
	}
	ns := opts.Namespace
	if ns == "" {
		ns = "federation-autoscaler-system"
	}
	shutdown := opts.ShutdownTimeout
	if shutdown <= 0 {
		shutdown = 5 * time.Second
	}
	s := &Server{
		role:     opts.Role,
		bind:     opts.BindAddress,
		local:    opts.LocalClient,
		ns:       ns,
		log:      logger,
		shutdown: shutdown,
	}
	s.srv = &http.Server{
		Addr:              opts.BindAddress,
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

// Handler returns the bare router. Exposed so tests can mount the server on an
// httptest.NewServer without binding a real socket.
func (s *Server) Handler() http.Handler { return s.handler() }

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	// Shared (both roles).
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /api/regions", s.handleRegions)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/region", s.handleRegion)
	// Role-gated writes. Only the active role's routes are registered, so the
	// other role's paths 404; handlers also re-check the role (defence in depth).
	switch s.role {
	case RoleConsumer:
		mux.HandleFunc("POST /api/policy", s.handlePolicy)
		mux.HandleFunc("POST /api/workload", s.handleWorkload)
	case RoleProvider:
		mux.HandleFunc("POST /api/prices", s.handlePrices)
		mux.HandleFunc("POST /api/capacity", s.handleCapacity)
	}
	return mux
}

// Run binds the listener and blocks until ctx is cancelled. Returns nil on
// graceful shutdown, the underlying error otherwise.
func (s *Server) Run(ctx context.Context) error {
	s.log.Info("starting agent config console (plain HTTP, no auth)",
		"role", s.role, "bindAddress", s.bind)

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
// Shared GET handlers
// -----------------------------------------------------------------------------

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// "GET /" is the catch-all for GET, so reject anything that is not exactly
	// the root (mirrors the broker dashboard's handleDashboardIndex).
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page := providerHTML
	if s.role == RoleConsumer {
		page = consumerHTML
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

func (s *Server) handleRegions(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"regions": regions.All})
}

// -----------------------------------------------------------------------------
// POST handlers
// -----------------------------------------------------------------------------

// handleRegion upserts the agent-location ConfigMap (both roles). An empty
// region clears it; a non-empty one must be a known code.
func (s *Server) handleRegion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Region string `json:"region"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.Region != "" && !regions.Valid(body.Region) {
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown region %q", body.Region))
		return
	}
	value := fmt.Sprintf("region: %q\n", body.Region)
	if err := s.upsertConfigMap(r.Context(), locationConfigMap, locationKey, value); err != nil {
		s.writeError(w, http.StatusInternalServerError, "write agent-location: "+err.Error())
		return
	}
	s.ok(w)
}

// handlePolicy creates/updates/deletes the `default` ConsumerPolicy. Empty or
// "None" deletes it (= no broker-driven preference).
func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request) {
	if s.role != RoleConsumer {
		s.writeError(w, http.StatusMethodNotAllowed, "policy is a consumer-only setting")
		return
	}
	var body struct {
		Type string `json:"type"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	switch t := body.Type; t {
	case "", "None":
		if err := s.deleteConsumerPolicy(r.Context()); err != nil {
			s.writeError(w, http.StatusInternalServerError, "delete ConsumerPolicy: "+err.Error())
			return
		}
	case string(autoscalingv1alpha1.PlacementStrategyPrice),
		string(autoscalingv1alpha1.PlacementStrategyEco),
		string(autoscalingv1alpha1.PlacementStrategyLatency):
		if err := s.upsertConsumerPolicy(r.Context(), autoscalingv1alpha1.PlacementStrategy(t)); err != nil {
			s.writeError(w, http.StatusInternalServerError, "write ConsumerPolicy: "+err.Error())
			return
		}
	default:
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown policy type %q", t))
		return
	}
	s.ok(w)
}

// handleWorkload applies or deletes the embedded federation-demo Deployment in
// the `default` namespace. Both verbs are idempotent.
func (s *Server) handleWorkload(w http.ResponseWriter, r *http.Request) {
	if s.role != RoleConsumer {
		s.writeError(w, http.StatusMethodNotAllowed, "workload is a consumer-only control")
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	dep, err := decodeWorkload()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "decode embedded workload: "+err.Error())
		return
	}
	switch body.Action {
	case "apply":
		if err := s.local.Create(r.Context(), dep); err != nil && !apierrors.IsAlreadyExists(err) {
			s.writeError(w, http.StatusInternalServerError, "apply workload: "+err.Error())
			return
		}
	case "delete":
		if err := s.local.Delete(r.Context(), dep); err != nil && !apierrors.IsNotFound(err) {
			s.writeError(w, http.StatusInternalServerError, "delete workload: "+err.Error())
			return
		}
	default:
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown action %q (want apply|delete)", body.Action))
		return
	}
	s.ok(w)
}

// handlePrices upserts the agent-prices ConfigMap. Values are unit prices
// (cpu per core-hour, memory per GiB-hour) validated as Kubernetes quantities.
func (s *Server) handlePrices(w http.ResponseWriter, r *http.Request) {
	if s.role != RoleProvider {
		s.writeError(w, http.StatusMethodNotAllowed, "prices is a provider-only setting")
		return
	}
	var body struct {
		CPU    string `json:"cpu"`
		Memory string `json:"memory"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.CPU == "" && body.Memory == "" {
		s.writeError(w, http.StatusBadRequest, "at least one of cpu/memory is required")
		return
	}
	lines := make([]string, 0, 2)
	for _, kv := range []struct{ name, val string }{{"cpu", body.CPU}, {"memory", body.Memory}} {
		if kv.val == "" {
			continue
		}
		if _, err := resource.ParseQuantity(kv.val); err != nil {
			s.writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid %s price %q: %v", kv.name, kv.val, err))
			return
		}
		lines = append(lines, fmt.Sprintf("%s: %q", kv.name, kv.val))
	}
	if err := s.upsertConfigMap(r.Context(), pricesConfigMap, pricesKey, joinLines(lines)); err != nil {
		s.writeError(w, http.StatusInternalServerError, "write agent-prices: "+err.Error())
		return
	}
	s.ok(w)
}

// handleCapacity upserts the agent-capacity ConfigMap. Values are integer
// percentages of allocatable, clamped to [0,100].
func (s *Server) handleCapacity(w http.ResponseWriter, r *http.Request) {
	if s.role != RoleProvider {
		s.writeError(w, http.StatusMethodNotAllowed, "capacity is a provider-only setting")
		return
	}
	var body struct {
		CPU    *int `json:"cpu"`
		Memory *int `json:"memory"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.CPU == nil && body.Memory == nil {
		s.writeError(w, http.StatusBadRequest, "at least one of cpu/memory is required")
		return
	}
	lines := make([]string, 0, 2)
	for _, kv := range []struct {
		name string
		val  *int
	}{{"cpu", body.CPU}, {"memory", body.Memory}} {
		if kv.val == nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %d", kv.name, clampPercent(*kv.val)))
	}
	if err := s.upsertConfigMap(r.Context(), capacityConfigMap, capacityKey, joinLines(lines)); err != nil {
		s.writeError(w, http.StatusInternalServerError, "write agent-capacity: "+err.Error())
		return
	}
	s.ok(w)
}

// -----------------------------------------------------------------------------
// Kubernetes write helpers
// -----------------------------------------------------------------------------

// upsertConfigMap sets data[key]=value on the named ConfigMap in the agent
// namespace, creating it if absent and preserving any other keys.
func (s *Server) upsertConfigMap(ctx context.Context, name, key, value string) error {
	var cm corev1.ConfigMap
	err := s.local.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: name}, &cm)
	if apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.ns},
			Data:       map[string]string{key: value},
		}
		return s.local.Create(ctx, &cm)
	}
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[key] = value
	return s.local.Update(ctx, &cm)
}

func (s *Server) upsertConsumerPolicy(ctx context.Context, t autoscalingv1alpha1.PlacementStrategy) error {
	var cp autoscalingv1alpha1.ConsumerPolicy
	err := s.local.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: consumerPolicyName}, &cp)
	if apierrors.IsNotFound(err) {
		cp = autoscalingv1alpha1.ConsumerPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: consumerPolicyName, Namespace: s.ns},
			Spec:       autoscalingv1alpha1.ConsumerPolicySpec{Placement: autoscalingv1alpha1.PlacementPolicy{Type: t}},
		}
		return s.local.Create(ctx, &cp)
	}
	if err != nil {
		return err
	}
	cp.Spec.Placement.Type = t
	return s.local.Update(ctx, &cp)
}

func (s *Server) deleteConsumerPolicy(ctx context.Context) error {
	cp := autoscalingv1alpha1.ConsumerPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: consumerPolicyName, Namespace: s.ns},
	}
	if err := s.local.Delete(ctx, &cp); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// joinLines renders ConfigMap value lines as a trailing-newline-terminated
// block (the shape the agent's loaders read back). Empty input yields "".
func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// clampPercent bounds an advertised-capacity percentage to [0,100].
func clampPercent(v int) int {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}

// -----------------------------------------------------------------------------
// HTTP helpers
// -----------------------------------------------------------------------------

// decode reads a JSON body into dst, writing a 400 and returning false on error.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		s.writeError(w, http.StatusBadRequest, "decode body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) ok(w http.ResponseWriter) {
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.V(1).Info("encode response failed", "err", err.Error())
	}
}
