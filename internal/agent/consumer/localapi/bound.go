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

package localapi

import (
	"fmt"
	"net/url"
	"strconv"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
)

// Query parameters of GET /local/nodegroups that carry a per-request latency
// bound (a ResourceRequest's spec.latencyBound). They override the
// ConsumerPolicy bound for that one call; the local brain applies them, the
// central Broker ignores them.
const (
	QueryMaxLatencyMs = "maxLatencyMs"
	QueryFromName     = "fromName"
	QueryFromLat      = "fromLat"
	QueryFromLon      = "fromLon"
)

// EncodeLatencyBound returns the query parameters for bound, or nil for a nil
// bound.
func EncodeLatencyBound(bound *autoscalingv1alpha1.LatencyBound) url.Values {
	if bound == nil {
		return nil
	}
	q := url.Values{}
	q.Set(QueryMaxLatencyMs, strconv.Itoa(int(bound.MaxLatencyMs)))
	if from := bound.From; from != nil {
		if from.Name != "" {
			q.Set(QueryFromName, from.Name)
		}
		q.Set(QueryFromLat, strconv.FormatFloat(from.Latitude, 'f', -1, 64))
		q.Set(QueryFromLon, strconv.FormatFloat(from.Longitude, 'f', -1, 64))
	}
	return q
}

// DecodeLatencyBound parses the latency-bound query parameters. It returns
// (nil, nil) when none are present, and an error for a malformed bound,
// applying the same limits as the CRD (maxLatencyMs >= 1, both coordinates
// together, latitude and longitude in range).
func DecodeLatencyBound(q url.Values) (*autoscalingv1alpha1.LatencyBound, error) {
	rawMax, rawLat, rawLon := q.Get(QueryMaxLatencyMs), q.Get(QueryFromLat), q.Get(QueryFromLon)
	if rawMax == "" {
		if rawLat != "" || rawLon != "" || q.Get(QueryFromName) != "" {
			return nil, fmt.Errorf("%s is required when a reference point is given", QueryMaxLatencyMs)
		}
		return nil, nil
	}
	ms, err := strconv.ParseInt(rawMax, 10, 32)
	if err != nil || ms < 1 {
		return nil, fmt.Errorf("%s must be an integer >= 1, got %q", QueryMaxLatencyMs, rawMax)
	}
	bound := &autoscalingv1alpha1.LatencyBound{MaxLatencyMs: int32(ms)}

	if rawLat == "" && rawLon == "" {
		return bound, nil // measured from the consumer's own location
	}
	if rawLat == "" || rawLon == "" {
		return nil, fmt.Errorf("%s and %s must be given together", QueryFromLat, QueryFromLon)
	}
	lat, err := strconv.ParseFloat(rawLat, 64)
	if err != nil || lat < -90 || lat > 90 {
		return nil, fmt.Errorf("%s must be a latitude in [-90, 90], got %q", QueryFromLat, rawLat)
	}
	lon, err := strconv.ParseFloat(rawLon, 64)
	if err != nil || lon < -180 || lon > 180 {
		return nil, fmt.Errorf("%s must be a longitude in [-180, 180], got %q", QueryFromLon, rawLon)
	}
	bound.From = &autoscalingv1alpha1.GeoPoint{Name: q.Get(QueryFromName), Latitude: lat, Longitude: lon}
	return bound, nil
}
