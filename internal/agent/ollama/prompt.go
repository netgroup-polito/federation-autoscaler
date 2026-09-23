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

package ollama

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// SystemPrompt is the fixed system-level instruction sent to Ollama on every
// ConsumerChoice selection call. It constrains the LLM to a ranking of the
// supplied provider IDs, returned as JSON and nothing else.
//
// The field glossary is part of the contract, not decoration: it states what
// each value measures, in which unit and which end is better, and which words
// of a request refer to it, so the model does not have to guess whether a
// larger carbonIntensity is better, what "green" is about, or that a farther
// provider is worse for proximity. It deliberately says nothing about how to
// weigh or combine the values, and computes nothing: which provider is near is
// left for the model to work out from the raw coordinates, told what they mean
// and given a rule to compare them by. That judgement is the model's job; the
// selector only supplies the data. The example numbers are not taken from any
// test catalogue.
//
// The proximity rule is deliberately the cheapest one that ranks: a sum of the
// two absolute differences, no trigonometry and no constant that only holds at
// some latitudes. Measured on llama3.2 it changes nothing, because that model
// does not compute at all -- asked for a proximityScore it writes each
// provider's carbonIntensity instead -- but a model that can compute has the
// method here rather than having to invent one.
//
// The user's request comes first. The cost-first defaults apply only to a
// request that states no preference and to ties on what the request asks for;
// without saying so, a small model applies them to every request and ranks a
// "greenest" request by cost. The schema has the model write, before the
// ranking, what the request asks for and the values it ranks by, copied from
// the list: ranking first and explaining after, llama3.2 ranked a "greenest"
// request by cost every time; writing the values first, it mostly did not.
const SystemPrompt = `You are a Kubernetes provider selection assistant.

You will receive a user request, the location of the consumer making the request (when known),
and a JSON list of available providers.
Rank ALL providers from best to worst match for the user's request.

Provider fields (a field is omitted when the provider does not advertise it):
- providerId: the identifier to return.
- carbonIntensity: grams of CO2 emitted per kWh by the provider's electricity grid (gCO2eq/kWh). Lower is greener: 50 is much greener than 500. The greenest provider is the one with the LOWEST carbonIntensity.
- costPerChunk: price of one chunk for one hour. Lower is cheaper. The cheapest provider is the one with the LOWEST costPerChunk.
- availableChunks: free capacity in chunks; availableCpuMillicores and availableMemoryMiB are the same capacity in CPU millicores and MiB.
- latitude, longitude: where the provider is, in decimal degrees; region: its region code. The closer a provider is to the consumer location, the better for proximity: the farther away, the worse. Coordinates within a few degrees of the consumer's are near; tens of degrees apart are far.

The consumer location, when present, gives the latitude and longitude (decimal degrees) and region code
of the consumer making the request, in the same form as the providers' location fields.

How words in the request map to fields:
- green, greenest, eco, clean, sustainable, low emissions, low carbon: carbonIntensity, lowest is best.
- cheap, cheapest, low cost, budget: costPerChunk, lowest is best.
- close, near, nearby, proximity, low latency: distance between the consumer location and the provider's latitude/longitude, shortest is best, farthest is worst.
- resources, capacity: availableChunks, highest is best.

When the request is about proximity, do not judge it by eye:
- for EVERY provider compute proximityScore = |provider latitude - consumer latitude| + |provider longitude - consumer longitude|;
- write each provider's proximityScore in "values";
- rank by proximityScore, smallest first.
It is a rough comparison between providers, not a distance in kilometres.

Rules:
- The user request has the highest priority: rank the providers by what it asks for, using the fields that measure it.
- Only if the request states no preference, prefer in order: lowest cost, lowest carbon, most resources.
- Only if two providers are equal for what the request asks, prefer the one with lower cost.
- Never let cost, or any default above, override a preference stated in the request.
- Choose only from the providers in the list.
- Never invent provider IDs or field values.
- Before ranking, copy from the list, for EVERY provider, the values of the fields the request is about; then rank by those values.
- Return only valid JSON matching this schema: {"reason": "<one short sentence: what the request asks for>", "values": [{"providerId": "<id>", "<field>": <value copied from the list>}, ...], "rankedList": ["<best>", "<2nd>", ...], "providerId": "<best>", "confidence": <number from 0 to 1>}
- rankedList must contain ALL provider IDs, each exactly once, ordered best first.
- providerId must equal the first element of rankedList.
- reason and confidence are optional.
- No markdown and no text outside the JSON object.`

// ProviderInfo is the structured JSON the LLM sees per provider. It contains
// only the fields relevant to a placement decision, flattened from the
// NodeGroupView wire type.
type ProviderInfo struct {
	ProviderID      string `json:"providerId"`
	NodeGroupID     string `json:"nodeGroupId,omitempty"`
	ClusterType     string `json:"clusterType"`
	AvailableChunks int32  `json:"availableChunks"`
	CPUPerChunk     string `json:"cpuPerChunk,omitempty"`
	MemoryPerChunk  string `json:"memoryPerChunk,omitempty"`
	GPUPerChunk     string `json:"gpuPerChunk,omitempty"`
	// AvailableCPUMillicores and AvailableMemoryMiB are AvailableChunks
	// expressed as plain numbers, so the model need not parse Kubernetes
	// quantity strings like "4Gi" to compare capacity.
	AvailableCPUMillicores int64    `json:"availableCpuMillicores,omitempty"`
	AvailableMemoryMiB     int64    `json:"availableMemoryMiB,omitempty"`
	CostPerChunk           *float64 `json:"costPerChunk,omitempty"`
	CarbonIntensity        *float64 `json:"carbonIntensity,omitempty"`
	Region                 string   `json:"region,omitempty"`
	Latitude               float64  `json:"latitude,omitempty"`
	Longitude              float64  `json:"longitude,omitempty"`
}

// Location is where the requesting consumer is: coordinates in decimal degrees
// and its region code, in the same form providers advertise theirs. Without it
// a request like "close to me" has no referent -- the providers' coordinates
// alone do not say where "me" is.
type Location struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Region    string  `json:"region,omitempty"`
}

// SelectionResponse is the JSON schema Ollama must return. The LLM is
// instructed to return a ranked list of ALL provider IDs; the single
// ProviderID field is kept for backward compatibility with older models
// that return only one.
//
// Reason and Confidence are the model's own account of its choice. They are
// kept for a human to read and are never used to decide anything: a model can
// state a fluent reason for a wrong answer. The "values" the model copies
// before ranking are its working, not part of the answer: they are not decoded
// and stay readable in the raw response.
type SelectionResponse struct {
	ProviderID string   `json:"providerId"`
	RankedList []string `json:"rankedList,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
}

// NodeGroupViewToProviderInfo converts a broker NodeGroupView into the
// simplified shape the LLM receives.
func NodeGroupViewToProviderInfo(ng brokerapi.NodeGroupView) ProviderInfo {
	info := ProviderInfo{
		ProviderID:      ng.ProviderClusterID,
		NodeGroupID:     ng.ID,
		ClusterType:     string(ng.Type),
		AvailableChunks: ng.MaxSize - ng.CurrentReserved,
	}

	// Extract per-chunk resources.
	if q, ok := ng.ChunkResources[corev1.ResourceCPU]; ok {
		info.CPUPerChunk = q.String()
		if info.AvailableChunks > 0 {
			info.AvailableCPUMillicores = int64(info.AvailableChunks) * q.MilliValue()
		}
	}
	if q, ok := ng.ChunkResources[corev1.ResourceMemory]; ok {
		info.MemoryPerChunk = q.String()
		if info.AvailableChunks > 0 {
			info.AvailableMemoryMiB = int64(info.AvailableChunks) * q.Value() / (1024 * 1024)
		}
	}
	if q, ok := ng.ChunkResources["nvidia.com/gpu"]; ok {
		info.GPUPerChunk = q.String()
	}

	// Cost: convert from *resource.Quantity to *float64 for JSON readability,
	// through the decimal form: AsApproximateFloat64 turns 52m into
	// 0.052000000000000005, and the model reads these numbers as text.
	// AsDec rewrites the quantity it is called on, hence the copy.
	if ng.Cost != nil {
		q := ng.Cost.DeepCopy()
		v, err := strconv.ParseFloat(q.AsDec().String(), 64)
		if err != nil {
			v = ng.Cost.AsApproximateFloat64()
		}
		info.CostPerChunk = &v
	}

	// Carbon and topology.
	info.CarbonIntensity = ng.CarbonIntensity
	if ng.Topology != nil {
		info.Region = ng.Topology.Region
		info.Latitude = ng.Topology.Latitude
		info.Longitude = ng.Topology.Longitude
	}

	return info
}

// BuildUserPrompt assembles the user-facing portion of the Ollama prompt from
// the user's natural-language request, the consumer's own location (omitted
// when nil) and the structured provider list.
//
// The request opens the prompt and is repeated after the provider list: that
// list is long, and a small model answers from what it read last.
func BuildUserPrompt(userRequest string, consumer *Location, providers []ProviderInfo) string {
	providersJSON, err := json.MarshalIndent(providers, "", "  ")
	if err != nil {
		// Fallback to compact JSON if indented fails (should never happen).
		providersJSON, _ = json.Marshal(providers)
	}

	var sb strings.Builder
	sb.WriteString("USER REQUEST:\n")
	sb.WriteString(fmt.Sprintf("%q\n\n", userRequest))
	if consumer != nil {
		locationJSON, _ := json.Marshal(consumer) // flat struct of numbers and a string: cannot fail
		sb.WriteString("CONSUMER LOCATION (where the request comes from):\n")
		sb.Write(locationJSON)
		sb.WriteString("\n\n")
	}
	sb.WriteString("AVAILABLE PROVIDERS:\n")
	sb.Write(providersJSON)
	sb.WriteString(fmt.Sprintf("\n\nRank ALL the providers above for this USER REQUEST: %q", userRequest))
	return sb.String()
}
