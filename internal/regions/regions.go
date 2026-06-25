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

// Package regions is the canonical list of region codes an operator can pick
// for a cluster's location. Selecting a region drives both placement
// strategies: the Eco strategy ranks providers by the region's carbon
// intensity, the Latency strategy by the great-circle distance between region
// coordinates. The agent writes the chosen code into the agent-location
// ConfigMap (key location.yaml: `region: "<code>"`); the mock-eco / mock-geo
// services then resolve that code to a carbon number / coordinates.
//
// The codes here MUST stay a subset of the keys the mock services know about
// (internal/mockgeo, internal/mockeco) — a code with no entry there resolves to
// no coordinates / no carbon and silently opts the cluster out of the strategy.
// This package is the single source of truth the config console offers in its
// region dropdown (GET /api/regions).
package regions

// Region is one selectable location: a short code (what gets written to the
// agent-location ConfigMap and sent on advertisements/heartbeats) plus a
// human-readable city used only for display in the console dropdown.
type Region struct {
	Code string `json:"code"`
	City string `json:"city"`
}

// All is the canonical, deterministically-ordered set of regions. The order is
// stable so the console dropdown and any test assertions are reproducible.
var All = []Region{
	{Code: "QC", City: "Montreal"},
	{Code: "IDF", City: "Paris"},
	{Code: "ENG", City: "London"},
	{Code: "LOM", City: "Milan"},
	{Code: "HE", City: "Frankfurt"},
	{Code: "CA", City: "San Jose"},
	{Code: "SP", City: "São Paulo"},
	{Code: "SG", City: "Singapore"},
	{Code: "13", City: "Tokyo"},
	{Code: "NSW", City: "Sydney"},
}

// Valid reports whether code is one of the known region codes.
func Valid(code string) bool {
	for i := range All {
		if All[i].Code == code {
			return true
		}
	}
	return false
}
