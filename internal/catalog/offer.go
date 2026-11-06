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

package catalog

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Offer is what one provider publishes for one site.
// A provider with a single site uses SiteID "default".
//
// Rules (catalog contract v1):
//   - Units are always Kubernetes units (cpu, memory, nvidia.com/gpu).
//   - Available is the capacity the provider can still give out right now.
//     The provider keeps its own counters and refreshes this field.
//   - The catalog stores what the provider writes and computes nothing
//     (no chunks, no ranking).
//   - Readers ignore unknown fields. New fields may be added, existing
//     fields are never renamed or removed. A breaking change means
//     SchemaVersion 2.
type Offer struct {
	SchemaVersion int    `json:"schemaVersion"` // always 1 for now
	ProviderID    string `json:"providerId"`    // = cluster ID (certificate CN)
	LiqoClusterID string `json:"liqoClusterId"`
	SiteID        string `json:"siteId"` // "asti", or "default"

	Location Location `json:"location"`

	// Available is the free capacity right now: cpu, memory, nvidia.com/gpu.
	Available corev1.ResourceList `json:"available"`

	// UnitPrices is EUR per unit per hour.
	UnitPrices corev1.ResourceList `json:"unitPrices,omitempty"`

	// SLA is a free label, e.g. "gold".
	SLA string `json:"sla,omitempty"`

	// ExpectedLatencyMs is declared by the provider.
	ExpectedLatencyMs *float64 `json:"expectedLatencyMs,omitempty"`

	// CarbonIntensity is in gCO2/kWh.
	CarbonIntensity *float64  `json:"carbonIntensity,omitempty"`
	CarbonForecast  []float64 `json:"carbonForecast,omitempty"`
	Renewable       bool      `json:"renewable,omitempty"`

	// ProbeEndpoint is host:port used for latency probing.
	ProbeEndpoint string `json:"probeEndpoint,omitempty"`

	// NodeLabels and NodeTaints describe the borrowed node.
	NodeLabels map[string]string `json:"nodeLabels,omitempty"`
	NodeTaints []corev1.Taint    `json:"nodeTaints,omitempty"`

	// Extra holds anything else, unstructured.
	Extra map[string]any `json:"extra,omitempty"`

	UpdatedAt time.Time `json:"updatedAt"`
}

// Location says where a site is.
type Location struct {
	Region    string   `json:"region,omitempty"` // e.g. "Piedmont"
	City      string   `json:"city,omitempty"`   // e.g. "Asti"
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
}
