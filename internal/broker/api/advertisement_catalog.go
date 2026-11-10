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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/broker/chunk"
	"github.com/netgroup-polito/federation-autoscaler/internal/catalog"
)

// catalogSiteID is the single site every provider has in Phase 1.
const catalogSiteID = "default"

// publishOffer writes the provider's offer to the catalog, if one is
// configured. It is best effort: a failure is logged at Info and never fails
// the advertisement, because the ClusterAdvertisement stays the source of
// truth and the next advertisement rewrites the offer.
func (s *Server) publishOffer(
	ctx context.Context,
	req *AdvertisementRequest,
	cadv *brokerv1alpha1.ClusterAdvertisement,
	res chunk.Result,
	requestID string,
) {
	if s.catalog == nil {
		return
	}
	offer := offerFromAdvertisement(req, cadv.Status.ReservedChunks, res, time.Now())
	if err := s.catalog.UpsertOffer(ctx, offer); err != nil {
		s.log.Info("catalog write failed (will retry next advertisement)",
			"err", err.Error(), "clusterId", req.ClusterID, "requestId", requestID)
	}
}

// offerFromAdvertisement maps an accepted advertisement to the catalog Offer.
// Available is the FREE capacity: the advertised resources minus
// reserved x PerChunk, per resource, never below zero.
func offerFromAdvertisement(
	req *AdvertisementRequest,
	reserved int32,
	res chunk.Result,
	now time.Time,
) catalog.Offer {
	return catalog.Offer{
		SchemaVersion:   1,
		ProviderID:      req.ClusterID,
		LiqoClusterID:   req.LiqoClusterID,
		SiteID:          catalogSiteID,
		Location:        locationFromTopology(req),
		Available:       freeCapacity(req.Resources, reserved, res.PerChunk),
		UnitPrices:      req.UnitPrices,
		CarbonIntensity: req.CarbonIntensity,
		CarbonForecast:  req.CarbonForecast,
		Renewable:       req.Renewable,
		ProbeEndpoint:   req.ProbeEndpoint,
		UpdatedAt:       now.UTC(),
	}
}

// locationFromTopology copies region and city, and the coordinates only when
// they are known (0,0 means "unknown").
func locationFromTopology(req *AdvertisementRequest) catalog.Location {
	t := req.Topology
	if t == nil {
		return catalog.Location{}
	}
	loc := catalog.Location{Region: t.Region, City: t.City}
	if t.Latitude != 0 || t.Longitude != 0 {
		lat, lon := t.Latitude, t.Longitude
		loc.Latitude, loc.Longitude = &lat, &lon
	}
	return loc
}

// freeCapacity returns adv minus reserved x perChunk, per resource, never
// below zero. Resources missing from perChunk are copied unchanged.
func freeCapacity(adv corev1.ResourceList, reserved int32, perChunk corev1.ResourceList) corev1.ResourceList {
	out := make(corev1.ResourceList, len(adv))
	for name, total := range adv {
		free := total.DeepCopy()
		if per, ok := perChunk[name]; ok && reserved > 0 {
			used := resource.NewMilliQuantity(per.MilliValue()*int64(reserved), per.Format)
			free.Sub(*used)
		}
		if free.Sign() < 0 {
			free = *resource.NewQuantity(0, total.Format)
		}
		out[name] = free
	}
	return out
}
