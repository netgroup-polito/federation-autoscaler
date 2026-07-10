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

package console

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
)

// consumerState is the GET /api/state body on a consumer; it lets the UI
// pre-select the current policy/region and reflect the workload switch.
type consumerState struct {
	Role               string                  `json:"role"`
	ClusterID          string                  `json:"clusterId"`
	LiqoClusterID      string                  `json:"liqoClusterId"`
	Policy             string                  `json:"policy"`
	Region             string                  `json:"region"`
	Workload           workloadInfo            `json:"workload"`
	ManualReservations []manualReservationInfo `json:"manualReservations"`
}

// manualReservationInfo is one console-managed ResourceRequest (manual
// reservation), surfaced so the UI can list active reservations and release a
// chosen one by name.
type manualReservationInfo struct {
	Name     string `json:"name"`
	Phase    string `json:"phase,omitempty"`
	Provider string `json:"provider,omitempty"`
	Chunks   int32  `json:"chunks,omitempty"`
	Message  string `json:"message,omitempty"`
	CPU      string `json:"cpu,omitempty"`
	Memory   string `json:"memory,omitempty"`
	GPU      string `json:"gpu,omitempty"`
}

// providerState is the GET /api/state body on a provider.
type providerState struct {
	Role          string            `json:"role"`
	ClusterID     string            `json:"clusterId"`
	LiqoClusterID string            `json:"liqoClusterId"`
	Prices        map[string]string `json:"prices"`
	Region        string            `json:"region"`
	Capacity      map[string]string `json:"capacity"`
	Renewable     bool              `json:"renewable"`
}

// handleState returns the current settings so the UI can pre-fill its controls.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	region := s.readRegion(ctx)
	if s.role == RoleConsumer {
		info, err := staticWorkloadInfo()
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "decode embedded workload: "+err.Error())
			return
		}
		info.Present = s.workloadPresent(ctx)
		s.writeJSON(w, http.StatusOK, consumerState{
			Role:               RoleConsumer,
			ClusterID:          s.clusterID,
			LiqoClusterID:      s.liqoClusterID,
			Policy:             s.readPolicy(ctx),
			Region:             region,
			Workload:           info,
			ManualReservations: s.listManualReservations(ctx),
		})
		return
	}
	s.writeJSON(w, http.StatusOK, providerState{
		Role:          RoleProvider,
		ClusterID:     s.clusterID,
		LiqoClusterID: s.liqoClusterID,
		Prices:        s.readPrices(ctx),
		Region:        region,
		Capacity:      s.readCapacity(ctx),
		Renewable:     s.readRenewable(ctx),
	})
}

// listManualReservations returns all console-managed ResourceRequests in the
// agent namespace (sorted by name) so the UI can list active reservations and
// release a chosen one. Hand-created ResourceRequests (no console label) are
// omitted — the console manages only what it created.
func (s *Server) listManualReservations(ctx context.Context) []manualReservationInfo {
	var list autoscalingv1alpha1.ResourceRequestList
	if err := s.local.List(ctx, &list,
		ctrlclient.InNamespace(s.ns),
		ctrlclient.MatchingLabels{consoleManagedLabel: consoleManagedValue}); err != nil {
		s.log.V(1).Info("list manual reservations failed", "err", err.Error())
		return nil
	}
	out := make([]manualReservationInfo, 0, len(list.Items))
	for i := range list.Items {
		rr := &list.Items[i]
		info := manualReservationInfo{
			Name:     rr.Name,
			Phase:    string(rr.Status.Phase),
			Provider: rr.Status.ProviderClusterID,
			Chunks:   rr.Status.ChunkCount,
			Message:  rr.Status.Message,
		}
		if q, ok := rr.Spec.Resources[corev1.ResourceCPU]; ok {
			info.CPU = q.String()
		}
		if q, ok := rr.Spec.Resources[corev1.ResourceMemory]; ok {
			info.Memory = q.String()
		}
		if q, ok := rr.Spec.Resources[manualReservationGPU]; ok {
			info.GPU = q.String()
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// readRenewable returns this provider's self-declared renewable flag from the
// agent-renewable ConfigMap, or false when unset/absent/unparseable.
func (s *Server) readRenewable(ctx context.Context) bool {
	raw := s.readConfigMapKey(ctx, renewableConfigMap, renewableKey)
	if raw == "" {
		return false
	}
	var doc struct {
		Renewable bool `json:"renewable"`
	}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		s.log.V(1).Info("agent-renewable unparseable", "err", err.Error())
		return false
	}
	return doc.Renewable
}

// readPolicy returns the current `default` ConsumerPolicy type, or "" when the
// CR is absent (= no preference).
func (s *Server) readPolicy(ctx context.Context) string {
	var cp autoscalingv1alpha1.ConsumerPolicy
	if err := s.local.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: consumerPolicyName}, &cp); err != nil {
		return ""
	}
	return string(cp.Spec.Placement.Type)
}

// readRegion returns the region currently in the agent-location ConfigMap, or
// "" when unset/absent.
func (s *Server) readRegion(ctx context.Context) string {
	raw := s.readConfigMapKey(ctx, locationConfigMap, locationKey)
	if raw == "" {
		return ""
	}
	var loc struct {
		Region string `json:"region"`
	}
	if err := yaml.Unmarshal([]byte(raw), &loc); err != nil {
		s.log.V(1).Info("agent-location unparseable", "err", err.Error())
		return ""
	}
	return strings.TrimSpace(loc.Region)
}

// readPrices returns the cpu/memory unit prices in the agent-prices ConfigMap as
// strings, or an empty map when unset/absent.
func (s *Server) readPrices(ctx context.Context) map[string]string {
	out := map[string]string{}
	raw := s.readConfigMapKey(ctx, pricesConfigMap, pricesKey)
	if raw == "" {
		return out
	}
	// Parse generically so a value written either quoted ("0.020") or bare
	// (0.020) round-trips; keep the literal so the UI shows what was set rather
	// than a Quantity-canonicalised form (e.g. "0.020" → "20m").
	var prices map[string]any
	if err := yaml.Unmarshal([]byte(raw), &prices); err != nil {
		s.log.V(1).Info("agent-prices unparseable", "err", err.Error())
		return out
	}
	for _, k := range []string{"cpu", "memory"} {
		if v, ok := prices[k]; ok && v != nil {
			out[k] = strings.TrimSpace(fmt.Sprintf("%v", v))
		}
	}
	return out
}

// readCapacity returns the cpu/memory advertised-capacity caps in the
// agent-capacity ConfigMap as their raw literal form — a percentage ("80" or
// "80%") or a fixed quantity ("8Gi") — or an empty map when unset/absent. The
// UI classifies percent-vs-fixed the same way the provider agent does.
func (s *Server) readCapacity(ctx context.Context) map[string]string {
	out := map[string]string{}
	raw := s.readConfigMapKey(ctx, capacityConfigMap, capacityKey)
	if raw == "" {
		return out
	}
	// Parse generically so a value written bare (cpu: 80), quoted (cpu: "80%"),
	// or as a quantity (memory: 8Gi) round-trips as the literal the operator set.
	var caps map[string]any
	if err := yaml.Unmarshal([]byte(raw), &caps); err != nil {
		s.log.V(1).Info("agent-capacity unparseable", "err", err.Error())
		return out
	}
	for _, k := range []string{"cpu", "memory"} {
		if v, ok := caps[k]; ok && v != nil {
			out[k] = strings.TrimSpace(fmt.Sprintf("%v", v))
		}
	}
	return out
}

// readConfigMapKey returns data[key] of the named ConfigMap, or "" when the
// ConfigMap or key is absent.
func (s *Server) readConfigMapKey(ctx context.Context, name, key string) string {
	var cm corev1.ConfigMap
	if err := s.local.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: name}, &cm); err != nil {
		if !apierrors.IsNotFound(err) {
			s.log.V(1).Info("read ConfigMap failed", "name", name, "err", err.Error())
		}
		return ""
	}
	return cm.Data[key]
}

// workloadPresent reports whether the federation-demo Deployment exists in the
// `default` namespace.
func (s *Server) workloadPresent(ctx context.Context) bool {
	var dep appsv1.Deployment
	if err := s.local.Get(ctx, types.NamespacedName{Namespace: workloadNamespace, Name: workloadName}, &dep); err != nil {
		if !apierrors.IsNotFound(err) {
			s.log.V(1).Info("read workload failed", "err", err.Error())
		}
		return false
	}
	return true
}
