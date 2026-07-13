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
//	  POST /api/workload  apply/delete the federation-demo burst workload
//	  POST /api/reservation apply/delete the console-managed ResourceRequest
//	provider role:
//	  POST /api/prices    upsert the agent-prices ConfigMap
//	  POST /api/capacity  upsert the agent-capacity ConfigMap
//	  POST /api/renewable upsert the agent-renewable ConfigMap
//	both roles:
//	  GET  /              the role's embedded single-page UI
//	  GET  /api/state     current values, incl. the auto-discovered location
//	                      (read-only; location is no longer operator-set)
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
	"strconv"
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
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/geo"
)

// Role discriminates which UI and which write endpoints the console exposes.
const (
	RoleConsumer = "consumer"
	RoleProvider = "provider"
)

// Resource coordinates the console reads from / writes to. The ConfigMap names
// and keys MUST match what the agent's loaders read (loadUnitPrices /
// loadCapacityPercents / loadRenewable) and what the per-role kustomize overlays mount.
const (
	pricesConfigMap    = "agent-prices"
	pricesKey          = "prices.yaml"
	capacityConfigMap  = "agent-capacity"
	capacityKey        = "capacity.yaml"
	renewableConfigMap = "agent-renewable"
	renewableKey       = "renewable.yaml"

	// The burst workload lives in `default` (offloaded LocalAndRemote), NOT in
	// the agent namespace — the consumer overlay's scoped Role grants the write.
	workloadNamespace = "default"
	workloadName      = "federation-demo"

	// consumerPolicyName is the single ConsumerPolicy the console manages. A
	// hand-created sibling CR would still influence placement; the console only
	// owns this one (the heartbeat picks the lowest-named CR).
	consumerPolicyName = "default"

	// manualReservationPrefix is the generateName prefix for console-created
	// ResourceRequests (each Reserve makes a new, uniquely-named one so several
	// can coexist and be released individually).
	manualReservationPrefix = "manual-res-"
	// consoleManagedLabel/Value tag the ResourceRequests the console creates, so
	// the release dropdown lists only those and a release never touches a
	// hand-created request.
	consoleManagedLabel = "federation-autoscaler.io/console"
	consoleManagedValue = "manual-reservation"
	// manualReservationGPU is the GPU resource key on a manual reservation.
	manualReservationGPU corev1.ResourceName = "nvidia.com/gpu"
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

	// ClusterID and LiqoClusterID identify this cluster for display in the
	// console header so an operator with several tabs open can tell them apart.
	// ClusterID is the broker-facing federation ID (--cluster-id, e.g.
	// "consumer-1"); LiqoClusterID is the Liqo UUID (--liqo-cluster-id) the
	// broker keys advertisements by. Display-only; both optional.
	ClusterID     string
	LiqoClusterID string

	// NodeName, AdvertisedIP, and MockGeoURL let the console show this cluster's
	// AUTO-DISCOVERED location read-only (same inputs the agent's poller uses):
	// the node IP (or AdvertisedIP override) is geolocated via MockGeoURL. All
	// optional; when unset the location card simply shows "not configured".
	NodeName     string
	AdvertisedIP string
	MockGeoURL   string

	// Logger is the structured logger every handler logs through. Defaults to
	// controller-runtime's logger named "console".
	Logger logr.Logger

	// ShutdownTimeout caps how long Run waits for in-flight requests to drain on
	// ctx cancellation. Defaults to 5 s.
	ShutdownTimeout time.Duration
}

// Server is the console HTTP listener.
type Server struct {
	role          string
	bind          string
	local         ctrlclient.Client
	ns            string
	clusterID     string
	liqoClusterID string
	nodeName      string
	advertisedIP  string
	mockGeoURL    string
	geoClient     *geo.Client
	log           logr.Logger
	shutdown      time.Duration

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
		role:          opts.Role,
		bind:          opts.BindAddress,
		local:         opts.LocalClient,
		ns:            ns,
		clusterID:     opts.ClusterID,
		liqoClusterID: opts.LiqoClusterID,
		nodeName:      opts.NodeName,
		advertisedIP:  opts.AdvertisedIP,
		mockGeoURL:    opts.MockGeoURL,
		geoClient:     geo.NewClient(),
		log:           logger,
		shutdown:      shutdown,
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
	mux.HandleFunc("GET /api/state", s.handleState)
	// Role-gated writes. Only the active role's routes are registered, so the
	// other role's paths 404; handlers also re-check the role (defence in depth).
	switch s.role {
	case RoleConsumer:
		mux.HandleFunc("POST /api/policy", s.handlePolicy)
		mux.HandleFunc("POST /api/workload", s.handleWorkload)
		mux.HandleFunc("POST /api/reservation", s.handleReservation)
	case RoleProvider:
		mux.HandleFunc("POST /api/prices", s.handlePrices)
		mux.HandleFunc("POST /api/capacity", s.handleCapacity)
		mux.HandleFunc("POST /api/renewable", s.handleRenewable)
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

// -----------------------------------------------------------------------------
// POST handlers
// -----------------------------------------------------------------------------

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

// handleReservation creates or releases a console-managed ResourceRequest
// (manual reservation). "apply" creates a NEW, uniquely-named request from the
// requested cpu/memory/gpu (so several can coexist) and the grpc-server
// controller reserves + peers it; "delete" releases the one named in the body
// (from the release dropdown) so the finalizer frees it. Consumer-only.
func (s *Server) handleReservation(w http.ResponseWriter, r *http.Request) {
	if s.role != RoleConsumer {
		s.writeError(w, http.StatusMethodNotAllowed, "manual reservation is a consumer-only action")
		return
	}
	var body struct {
		Action   string `json:"action"`
		Name     string `json:"name"`
		CPU      string `json:"cpu"`
		Memory   string `json:"memory"`
		GPU      string `json:"gpu"`
		Duration string `json:"duration"`
		Priority int32  `json:"priority"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	switch body.Action {
	case "apply":
		res := corev1.ResourceList{}
		for _, kv := range []struct {
			name corev1.ResourceName
			val  string
		}{{corev1.ResourceCPU, body.CPU}, {corev1.ResourceMemory, body.Memory}, {manualReservationGPU, body.GPU}} {
			v := strings.TrimSpace(kv.val)
			if v == "" {
				continue
			}
			q, err := resource.ParseQuantity(v)
			if err != nil {
				s.writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid %s quantity %q: %v", kv.name, v, err))
				return
			}
			res[kv.name] = q
		}
		if len(res) == 0 {
			s.writeError(w, http.StatusBadRequest, "at least one of cpu/memory/gpu is required")
			return
		}
		if err := s.createManualReservation(r.Context(), res, strings.TrimSpace(body.Duration), body.Priority); err != nil {
			s.writeError(w, http.StatusInternalServerError, "apply reservation: "+err.Error())
			return
		}
	case "delete":
		name := strings.TrimSpace(body.Name)
		if name == "" {
			s.writeError(w, http.StatusBadRequest, "name is required to release a reservation")
			return
		}
		if err := s.releaseManualReservation(r.Context(), name); err != nil {
			s.writeError(w, http.StatusInternalServerError, "release reservation: "+err.Error())
			return
		}
	default:
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown action %q (want apply|delete)", body.Action))
		return
	}
	s.ok(w)
}

// createManualReservation creates a new, uniquely-named, console-labelled
// ResourceRequest so multiple manual reservations can coexist.
func (s *Server) createManualReservation(ctx context.Context, res corev1.ResourceList, duration string, priority int32) error {
	rr := &autoscalingv1alpha1.ResourceRequest{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: manualReservationPrefix,
			Namespace:    s.ns,
			Labels:       map[string]string{consoleManagedLabel: consoleManagedValue},
		},
		Spec: autoscalingv1alpha1.ResourceRequestSpec{Resources: res, Duration: duration, Priority: priority},
	}
	return s.local.Create(ctx, rr)
}

// releaseManualReservation deletes the named ResourceRequest, but only if the
// console created it (label guard) — so a crafted request can't release a
// hand-created one. Deleting a not-found request is a no-op.
func (s *Server) releaseManualReservation(ctx context.Context, name string) error {
	var rr autoscalingv1alpha1.ResourceRequest
	err := s.local.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: name}, &rr)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if rr.Labels[consoleManagedLabel] != consoleManagedValue {
		return fmt.Errorf("reservation %q is not managed by the console", name)
	}
	return s.local.Delete(ctx, &rr)
}

// handleRenewable upserts the agent-renewable ConfigMap with this provider's
// self-declared renewable-energy flag (the standard composite policy's bonus
// input). Provider-only.
func (s *Server) handleRenewable(w http.ResponseWriter, r *http.Request) {
	if s.role != RoleProvider {
		s.writeError(w, http.StatusMethodNotAllowed, "renewable is a provider-only setting")
		return
	}
	var body struct {
		Renewable bool `json:"renewable"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	value := fmt.Sprintf("renewable: %t\n", body.Renewable)
	if err := s.upsertConfigMap(r.Context(), renewableConfigMap, renewableKey, value); err != nil {
		s.writeError(w, http.StatusInternalServerError, "write agent-renewable: "+err.Error())
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

// handleCapacity upserts the agent-capacity ConfigMap. Each resource's value is
// EITHER a percentage of allocatable (a bare integer or "N%", clamped to
// [0,100]) OR a fixed absolute Kubernetes quantity (e.g. "8Gi", "4000m"), so an
// operator can cap by fraction or by absolute amount, per resource.
func (s *Server) handleCapacity(w http.ResponseWriter, r *http.Request) {
	if s.role != RoleProvider {
		s.writeError(w, http.StatusMethodNotAllowed, "capacity is a provider-only setting")
		return
	}
	var body struct {
		CPU    string `json:"cpu"`
		Memory string `json:"memory"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	body.CPU = strings.TrimSpace(body.CPU)
	body.Memory = strings.TrimSpace(body.Memory)
	if body.CPU == "" && body.Memory == "" {
		s.writeError(w, http.StatusBadRequest, "at least one of cpu/memory is required")
		return
	}
	lines := make([]string, 0, 2)
	for _, kv := range []struct{ name, val string }{{"cpu", body.CPU}, {"memory", body.Memory}} {
		if kv.val == "" {
			continue
		}
		line, err := capacityLine(kv.name, kv.val)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		lines = append(lines, line)
	}
	if err := s.upsertConfigMap(r.Context(), capacityConfigMap, capacityKey, joinLines(lines)); err != nil {
		s.writeError(w, http.StatusInternalServerError, "write agent-capacity: "+err.Error())
		return
	}
	s.ok(w)
}

// capacityLine validates one capacity value and renders its agent-capacity line.
// A bare integer or "N%" is a percentage, clamped to [0,100] and written bare;
// any other value must be a valid Kubernetes quantity, written quoted so YAML
// preserves it as the string the provider agent reads as a fixed cap.
func capacityLine(name, val string) (string, error) {
	if pct, ok, err := parseCapacityPercent(val); ok {
		if err != nil {
			return "", fmt.Errorf("invalid %s percentage %q: %v", name, val, err)
		}
		return fmt.Sprintf("%s: %d", name, clampPercent(pct)), nil
	}
	if _, err := resource.ParseQuantity(val); err != nil {
		return "", fmt.Errorf("invalid %s capacity %q: not a percentage (e.g. 80 or \"80%%\") or a Kubernetes quantity (e.g. \"8Gi\")", name, val)
	}
	return fmt.Sprintf("%s: %q", name, val), nil
}

// parseCapacityPercent reports whether val is a percentage form ("N" or "N%")
// and, if so, its integer value. ok=false means val is not a percentage and
// should be treated as a fixed quantity. It mirrors the provider agent's own
// percent-vs-fixed rule so the console and the agent agree.
func parseCapacityPercent(val string) (pct int, ok bool, err error) {
	if t, has := strings.CutSuffix(val, "%"); has {
		n, e := strconv.Atoi(strings.TrimSpace(t))
		return n, true, e
	}
	if n, e := strconv.Atoi(val); e == nil {
		return n, true, nil
	}
	return 0, false, nil
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
