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
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/broker/chunk"
	"github.com/netgroup-polito/federation-autoscaler/internal/broker/store"
)

// This file translates an advertisement request into the Store's domain
// type. It lives apart from advertisement.go so the handler itself gains
// only the call, not the translation.

// offerFromRequest builds the Offer that an accepted advertisement records.
//
// Two fields are deliberately left zero. ReservedChunks belongs to the
// reservation path and UpsertOffer refuses to write it anyway; LastSeen is
// stamped by the store itself, so a clock skew here could never write a
// stale timestamp in.
func offerFromRequest(req *AdvertisementRequest, chunkResult chunk.Result) store.Offer {
	o := store.Offer{
		ClusterID:       req.ClusterID,
		LiqoClusterID:   req.LiqoClusterID,
		ClusterType:     string(chunkResult.Type),
		Topology:        topologyFromRequest(req.Topology),
		Resources:       resourcesFromRequest(req),
		ProbeEndpoint:   req.ProbeEndpoint,
		CarbonIntensity: req.CarbonIntensity,
		Renewable:       req.Renewable,

		// The provider advertises resources; how many chunks they are
		// worth is the Broker's own arithmetic, already done by the
		// sizer before this point.
		TotalChunks: chunkResult.Count,
	}

	if req.CarbonForecast != nil {
		o.CarbonForecast = append([]float64(nil), req.CarbonForecast...)
	}
	return o
}

// resourcesFromRequest folds the request's four per-resource maps into the
// single slice the domain type uses, keyed by the names present in
// Resources. A price, cap or percentage for a resource that was not
// advertised is dropped: Resources is authoritative, exactly as it is for
// the custom resource.
func resourcesFromRequest(req *AdvertisementRequest) []store.Resource {
	if len(req.Resources) == 0 {
		return nil
	}

	out := make([]store.Resource, 0, len(req.Resources))
	for name, q := range req.Resources {
		r := store.Resource{Name: string(name), Allocatable: q.String()}

		if p, ok := req.UnitPrices[name]; ok {
			v := p.String()
			r.UnitPrice = &v
		}
		if c, ok := req.CapacityFixed[name]; ok {
			v := c.String()
			r.FixedCap = &v
		}
		if pct, ok := req.CapacityScalePercent[name]; ok {
			v := pct
			r.ScalePercent = &v
		}
		out = append(out, r)
	}
	return out
}

// topologyFromRequest mirrors how location is read elsewhere: a provider
// has one only when the topology is present and the coordinates are not
// both zero, since 0,0 cannot be told apart from "unset".
func topologyFromRequest(t *brokerv1alpha1.Topology) store.Topology {
	if t == nil {
		return store.Topology{}
	}
	return store.Topology{
		Zone:        t.Zone,
		Region:      t.Region,
		City:        t.City,
		Latitude:    t.Latitude,
		Longitude:   t.Longitude,
		HasLocation: t.Latitude != 0 || t.Longitude != 0,
	}
}
