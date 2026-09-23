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

package main

import (
	"fmt"
	"math"
	"sort"

	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// CandidateMetrics is one eligible provider as the model saw it, plus its
// ranks among the other candidates. Built from the exact ProviderInfo list
// sent to the model, so the evaluation judges the choice against the data the
// choice was made on -- not against a fresher read that might differ.
type CandidateMetrics struct {
	ProviderID             string   `json:"providerId"`
	NodeGroupID            string   `json:"nodeGroupId"`
	ChunkType              string   `json:"chunkType"`
	Region                 string   `json:"region,omitempty"`
	Latitude               float64  `json:"latitude,omitempty"`
	Longitude              float64  `json:"longitude,omitempty"`
	AvailableChunks        int32    `json:"availableChunks"`
	AvailableCPUMillicores int64    `json:"availableCpuMillicores"`
	AvailableMemoryMiB     int64    `json:"availableMemoryMiB"`
	CostPerChunk           *float64 `json:"costPerChunk,omitempty"`
	CarbonIntensity        *float64 `json:"carbonIntensity,omitempty"`
	// DistanceKm is computed by the harness, only to judge the choice. It is
	// never sent to the model, which gets the raw coordinates of the consumer
	// and of every provider and has to judge proximity from them itself.
	DistanceKm *float64 `json:"distanceKm,omitempty"`
	// Ranks are 1 for the best (lowest) value; tied values share the lower
	// rank; 0 means the provider does not advertise that metric.
	CarbonRank    int      `json:"carbonRank"`
	DistanceRank  int      `json:"distanceRank"`
	CostRank      int      `json:"costRank"`
	ReferenceRank int      `json:"referenceRank,omitempty"`
	Reference     *float64 `json:"referenceScore,omitempty"`
}

// buildCandidateMetrics ranks the candidates the model was shown, measuring
// their distance from consumer (nil when unknown) for the evaluation.
func buildCandidateMetrics(providers []ollama.ProviderInfo, consumer *ollama.Location,
	weights *Weights) []CandidateMetrics {
	return candidateMetrics(providers, distancesFrom(consumer, providers), weights)
}

// distancesFrom is the great-circle distance from the consumer to each provider,
// with the formula the Broker's Latency policy ranks on, rounded to 0.1 km. An
// entry is nil when the consumer location is unknown or the provider has no
// coordinates ((0,0) is the zero value of an unset position, not a real site).
func distancesFrom(consumer *ollama.Location, providers []ollama.ProviderInfo) []*float64 {
	out := make([]*float64, len(providers))
	if consumer == nil {
		return out
	}
	for i, p := range providers {
		if p.Latitude == 0 && p.Longitude == 0 {
			continue
		}
		d := brokerapi.HaversineKm(consumer.Latitude, consumer.Longitude, p.Latitude, p.Longitude)
		d = math.Round(d*10) / 10
		out[i] = &d
	}
	return out
}

// candidateMetrics ranks the candidates on every metric, and on the reference
// score when weights are given. distances[i] belongs to providers[i].
func candidateMetrics(providers []ollama.ProviderInfo, distances []*float64, weights *Weights) []CandidateMetrics {
	out := make([]CandidateMetrics, len(providers))
	carbon := make([]*float64, len(providers))
	distance := distances
	cost := make([]*float64, len(providers))
	for i, p := range providers {
		out[i] = CandidateMetrics{
			ProviderID:             p.ProviderID,
			NodeGroupID:            p.NodeGroupID,
			ChunkType:              p.ClusterType,
			Region:                 p.Region,
			Latitude:               p.Latitude,
			Longitude:              p.Longitude,
			AvailableChunks:        p.AvailableChunks,
			AvailableCPUMillicores: p.AvailableCPUMillicores,
			AvailableMemoryMiB:     p.AvailableMemoryMiB,
			CostPerChunk:           p.CostPerChunk,
			CarbonIntensity:        p.CarbonIntensity,
			DistanceKm:             distance[i],
		}
		carbon[i], cost[i] = p.CarbonIntensity, p.CostPerChunk
	}
	for i, r := range rankAscending(carbon) {
		out[i].CarbonRank = r
	}
	for i, r := range rankAscending(distance) {
		out[i].DistanceRank = r
	}
	for i, r := range rankAscending(cost) {
		out[i].CostRank = r
	}
	if weights != nil {
		scores := referenceScores(carbon, distance, cost, *weights)
		for i, r := range rankAscending(scores) {
			out[i].Reference = scores[i]
			out[i].ReferenceRank = r
		}
	}
	return out
}

// rankAscending gives competition ranks ("1224"): the lowest value is 1, tied
// values share the lower rank, and the next distinct value skips accordingly.
// A nil value is not ranked (0) and does not occupy a rank.
func rankAscending(values []*float64) []int {
	type item struct {
		idx int
		v   float64
	}
	var present []item
	for i, v := range values {
		if v != nil {
			present = append(present, item{i, *v})
		}
	}
	sort.SliceStable(present, func(a, b int) bool { return present[a].v < present[b].v })
	ranks := make([]int, len(values))
	for pos, it := range present {
		if pos > 0 && it.v == present[pos-1].v {
			ranks[it.idx] = ranks[present[pos-1].idx]
			continue
		}
		ranks[it.idx] = pos + 1
	}
	return ranks
}

// referenceScores is a transparent weighted sum of min-max normalised metrics
// (0 = best candidate on that metric, 1 = worst). A candidate missing a
// weighted metric scores 1 on it: not advertising a value is never rewarded.
// It exists to give a reproducible yardstick, not to define the right answer.
func referenceScores(carbon, distance, cost []*float64, w Weights) []*float64 {
	norm := func(vals []*float64) []float64 {
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, v := range vals {
			if v != nil {
				lo, hi = math.Min(lo, *v), math.Max(hi, *v)
			}
		}
		out := make([]float64, len(vals))
		for i, v := range vals {
			switch {
			case v == nil:
				out[i] = 1
			case hi == lo:
				out[i] = 0
			default:
				out[i] = (*v - lo) / (hi - lo)
			}
		}
		return out
	}
	nc, nd, nk := norm(carbon), norm(distance), norm(cost)
	total := w.Carbon + w.Distance + w.Cost
	scores := make([]*float64, len(carbon))
	for i := range scores {
		s := (w.Carbon*nc[i] + w.Distance*nd[i] + w.Cost*nk[i]) / total
		s = math.Round(s*1e4) / 1e4
		scores[i] = &s
	}
	return scores
}

// inWorstQuantile reports whether rank lies among the ceil(q*n) worst ranks.
// With n = 9 and q = 0.25 that is ranks 7, 8 and 9.
func inWorstQuantile(rank, n int, q float64) bool {
	if rank <= 0 || n <= 0 {
		return false
	}
	return rank > n-int(math.Ceil(q*float64(n)))
}

// CriterionResult is the scenario-specific verdict on the final selection.
// Passed is nil when the criterion could not be evaluated (a metric it needs
// was not advertised), which is reported as such rather than as a pass or fail.
type CriterionResult struct {
	Type    string   `json:"type"`
	Passed  *bool    `json:"passed"`
	Details []string `json:"details"`
}

// evaluateCriterion judges selectedID against the scenario's rule using only
// measurable properties. The model's stated reason plays no part in it.
func evaluateCriterion(c Criterion, candidates []CandidateMetrics, selectedID string) CriterionResult {
	res := CriterionResult{Type: c.Type}
	var sel *CandidateMetrics
	for i := range candidates {
		if candidates[i].ProviderID == selectedID {
			sel = &candidates[i]
			break
		}
	}
	if sel == nil {
		res.Details = append(res.Details, fmt.Sprintf("selected provider %q is not among the candidates", selectedID))
		return res.with(false)
	}
	n := len(candidates)

	switch c.Type {
	case criterionEco:
		if sel.CarbonRank == 0 {
			res.Details = append(res.Details, "selected provider advertises no carbon intensity")
			return res
		}
		res.Details = append(res.Details,
			fmt.Sprintf("carbon rank %d of %d (must be <= %d)", sel.CarbonRank, n, c.MaxCarbonRank))
		return res.with(sel.CarbonRank <= c.MaxCarbonRank)

	case criterionProximity:
		if sel.DistanceRank == 0 || sel.CarbonRank == 0 {
			res.Details = append(res.Details, "distance or carbon not available for the selected provider")
			return res
		}
		near := sel.DistanceRank <= c.MaxDistanceRank
		res.Details = append(res.Details,
			fmt.Sprintf("distance rank %d of %d (must be <= %d)", sel.DistanceRank, n, c.MaxDistanceRank))
		outlier := false
		if inWorstQuantile(sel.CarbonRank, n, c.WorstQuantile) {
			// Only an outlier if a cleaner provider was also near: when every
			// nearby option is dirty, picking one of them is the trade-off the
			// request asked for, not a mistake.
			for _, other := range candidates {
				if other.ProviderID != sel.ProviderID && other.DistanceRank > 0 && other.DistanceRank <= c.MaxDistanceRank &&
					other.CarbonRank > 0 && other.CarbonRank < sel.CarbonRank {
					outlier = true
					res.Details = append(res.Details, fmt.Sprintf(
						"carbon rank %d is in the worst %.0f%% while nearby %s has carbon rank %d",
						sel.CarbonRank, c.WorstQuantile*100, other.ProviderID, other.CarbonRank))
					break
				}
			}
		}
		if !outlier {
			res.Details = append(res.Details,
				fmt.Sprintf("carbon rank %d is not a high-carbon outlier among nearby providers", sel.CarbonRank))
		}
		return res.with(near && !outlier)

	case criterionBalanced:
		if sel.DistanceRank == 0 || sel.CarbonRank == 0 {
			res.Details = append(res.Details, "distance or carbon not available for the selected provider")
			return res
		}
		worstCarbon := inWorstQuantile(sel.CarbonRank, n, c.WorstQuantile)
		worstDistance := inWorstQuantile(sel.DistanceRank, n, c.WorstQuantile)
		res.Details = append(res.Details,
			fmt.Sprintf("carbon rank %d of %d (worst %.0f%%: %v)", sel.CarbonRank, n, c.WorstQuantile*100, worstCarbon),
			fmt.Sprintf("distance rank %d of %d (worst %.0f%%: %v)", sel.DistanceRank, n, c.WorstQuantile*100, worstDistance))
		return res.with(!worstCarbon && !worstDistance)

	default: // criterionNone
		res.Details = append(res.Details, "no preference criterion: the selection only has to be valid and eligible")
		if sel.CostRank > 0 {
			// Informational: the system prompt tells the model to prefer the
			// lowest cost when a request is vague.
			res.Details = append(res.Details, fmt.Sprintf(
				"cost rank %d of %d (the prompt's rule for vague requests prefers cost rank 1)", sel.CostRank, n))
		}
		return res.with(true)
	}
}

func (r CriterionResult) with(passed bool) CriterionResult {
	r.Passed = &passed
	return r
}
