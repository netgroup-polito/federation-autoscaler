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

// QueryMaxLatencyMs is the query parameter of GET /local/nodegroups that
// carries a per-request latency bound (a ResourceRequest's
// spec.latencyBound). It overrides the ConsumerPolicy bound for that one call;
// the local brain applies it, the central Broker ignores it.
const QueryMaxLatencyMs = "maxLatencyMs"

// EncodeLatencyBound returns the query parameters for bound, or nil for a nil
// bound.
func EncodeLatencyBound(bound *autoscalingv1alpha1.LatencyBound) url.Values {
	if bound == nil {
		return nil
	}
	return url.Values{QueryMaxLatencyMs: {strconv.Itoa(int(bound.MaxLatencyMs))}}
}

// DecodeLatencyBound parses the latency-bound query parameter. It returns
// (nil, nil) when it is absent, and an error for a malformed bound, applying
// the same limit as the CRD (maxLatencyMs >= 1).
func DecodeLatencyBound(q url.Values) (*autoscalingv1alpha1.LatencyBound, error) {
	rawMax := q.Get(QueryMaxLatencyMs)
	if rawMax == "" {
		return nil, nil
	}
	ms, err := strconv.ParseInt(rawMax, 10, 32)
	if err != nil || ms < 1 {
		return nil, fmt.Errorf("%s must be an integer >= 1, got %q", QueryMaxLatencyMs, rawMax)
	}
	return &autoscalingv1alpha1.LatencyBound{MaxLatencyMs: int32(ms)}, nil
}
