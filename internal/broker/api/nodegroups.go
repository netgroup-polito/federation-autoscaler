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
	"net/http"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// -----------------------------------------------------------------------------
// 7.3.4 — GET /api/v1/nodegroups   (Consumer Agent → Broker)
// -----------------------------------------------------------------------------
//
// Step 5a deliberately keeps this endpoint dumb: it lists every
// ClusterAdvertisement in the namespace, drops those whose status.available
// is false, and returns one NodeGroupView per advertisement (each CR carries
// exactly one ChunkType so the "(provider × chunkType)" pair is implicit).
// No scoring, ranking, filtering by chunk type, or caller-side selection is
// performed — the consumer agent / gRPC server / Cluster Autoscaler stay in
// charge of choosing where to schedule.
//
// Sort order is stable: (clusterId ASC, chunkType ASC). CA's expander is the
// real decision maker; the broker's order only affects tie-breaks and log
// readability.

func (s *Server) handleNodeGroupsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := r.Header.Get(HeaderRequestID)

	var list brokerv1alpha1.ClusterAdvertisementList
	if err := s.client.List(ctx, &list, client.InNamespace(s.namespace)); err != nil {
		s.log.Error(err, "list ClusterAdvertisements failed", "requestId", requestID)
		writeError(w, http.StatusInternalServerError, ErrorResponse{
			Code:      ErrCodeInternalError,
			Message:   "advertisement list failed",
			RequestID: requestID,
		})
		return
	}

	views := make([]NodeGroupView, 0, len(list.Items))
	for i := range list.Items {
		cadv := &list.Items[i]
		if !cadv.Status.Available {
			continue
		}
		views = append(views, s.nodeGroupViewFromAdvertisement(cadv))
	}

	sort.SliceStable(views, func(i, j int) bool {
		if views[i].ProviderClusterID != views[j].ProviderClusterID {
			return views[i].ProviderClusterID < views[j].ProviderClusterID
		}
		return string(views[i].Type) < string(views[j].Type)
	})

	writeJSON(w, http.StatusOK, NodeGroupListResponse{
		NodeGroups:      views,
		Generation:      0, // step 5b will surface a real revision once the reconciler watches CRs.
		ServedAt:        metav1.Now(),
		CacheAgeSeconds: 0,
	})
}

// nodeGroupViewFromAdvertisement projects one ClusterAdvertisement onto the
// wire shape consumed by the Consumer Agent (and ultimately by the Cluster
// Autoscaler via the gRPC server).
//
// MinSize is hard-coded to 0 — chunks are always optional. MaxSize is the
// provider's total chunk count (Option B in docs/design.md): the consumer
// can reserve up to that, no further. CurrentReserved tracks how many of
// those chunks are already bound to active reservations.
//
// ChunkResources is recovered by re-running the configured sizer over the
// advertisement so callers see the same per-chunk shape that downstream
// node templates will report.
func (s *Server) nodeGroupViewFromAdvertisement(cadv *brokerv1alpha1.ClusterAdvertisement) NodeGroupView {
	return NodeGroupView{
		ID:                    nodeGroupID(cadv.Spec.ClusterID, cadv.Spec.ClusterType),
		ProviderClusterID:     cadv.Spec.ClusterID,
		ProviderLiqoClusterID: cadv.Spec.LiqoClusterID,
		Type:                  cadv.Spec.ClusterType,
		MinSize:               0,
		MaxSize:               cadv.Status.TotalChunks,
		CurrentReserved:       cadv.Status.ReservedChunks,
		ChunkResources:        s.perChunkResources(cadv),
		Cost:                  cadv.Spec.Price,
		Topology:              cadv.Spec.Topology,
		// LiqoLabels / LiqoTaints aren't on ClusterAdvertisementSpec yet
		// (designed but pending). When they land, populate Labels /
		// Taints straight from the spec; for now, leave them nil.
	}
}
