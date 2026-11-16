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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
)

// Query parameters of GET /local/nodegroups that describe one
// ResourceRequest, besides the latency bound (QueryMaxLatencyMs):
//   - QueryPrompt: the request's own plain-language text; the local AI ranks
//     the providers for it, whatever the ConsumerPolicy says.
//   - QueryNeedCPU / QueryNeedMemory / QueryNeedGPU: the request's size, so
//     the choice skips providers that cannot hold it.
const (
	QueryPrompt     = "prompt"
	QueryNeedCPU    = "needCpu"
	QueryNeedMemory = "needMemory"
	QueryNeedGPU    = "needGpu"
)

// maxQueryPromptLen matches the CRD limit on ResourceRequest.spec.userPrompt.
const maxQueryPromptLen = 2048

// NodeGroupQuery is what one caller of GET /local/nodegroups asks for. The
// zero value is the Cluster Autoscaler's plain read.
type NodeGroupQuery struct {
	// Bound is a per-request latency bound (nil: the ConsumerPolicy's).
	Bound *autoscalingv1alpha1.LatencyBound
	// Prompt is a per-request plain-language request ("": the ConsumerPolicy's).
	Prompt string
	// Need is the request's size (empty: any free capacity will do).
	Need corev1.ResourceList
}

var needParams = []struct {
	param string
	name  corev1.ResourceName
}{
	{QueryNeedCPU, corev1.ResourceCPU},
	{QueryNeedMemory, corev1.ResourceMemory},
	{QueryNeedGPU, corev1.ResourceName("nvidia.com/gpu")},
}

// EncodeNodeGroupQuery returns the query parameters for q, or nil for the
// zero query.
func EncodeNodeGroupQuery(q NodeGroupQuery) url.Values {
	v := EncodeLatencyBound(q.Bound)
	if v == nil {
		v = url.Values{}
	}
	if q.Prompt != "" {
		v.Set(QueryPrompt, q.Prompt)
	}
	for _, p := range needParams {
		if qty, ok := q.Need[p.name]; ok && !qty.IsZero() {
			v.Set(p.param, qty.String())
		}
	}
	if len(v) == 0 {
		return nil
	}
	return v
}

// DecodeNodeGroupQuery parses the parameters EncodeNodeGroupQuery writes. A
// malformed value is an error (the caller answers 400).
func DecodeNodeGroupQuery(v url.Values) (NodeGroupQuery, error) {
	var q NodeGroupQuery
	bound, err := DecodeLatencyBound(v)
	if err != nil {
		return q, err
	}
	q.Bound = bound
	q.Prompt = v.Get(QueryPrompt)
	if len(q.Prompt) > maxQueryPromptLen {
		return q, fmt.Errorf("%s longer than %d bytes", QueryPrompt, maxQueryPromptLen)
	}
	for _, p := range needParams {
		raw := v.Get(p.param)
		if raw == "" {
			continue
		}
		qty, err := resource.ParseQuantity(raw)
		if err != nil || qty.Sign() < 0 {
			return q, fmt.Errorf("%s must be a non-negative quantity, got %q", p.param, raw)
		}
		if q.Need == nil {
			q.Need = corev1.ResourceList{}
		}
		q.Need[p.name] = qty
	}
	return q, nil
}
