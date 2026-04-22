/*
Copyright 2026.

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

// This file holds types that are shared between more than one CRD in the
// broker.federation-autoscaler.io group (currently ClusterAdvertisement and
// Reservation). Keep only truly shared types here; per-CRD types live in each
// CRD's own *_types.go file.

package v1alpha1

// ChunkType identifies the category of a capacity chunk.
//
// A provider is advertised as "gpu" when it exposes nvidia.com/gpu > 0,
// otherwise "standard". A Reservation always targets exactly one ChunkType,
// and the Broker calculates chunk sizes from the chunk-config ConfigMap
// accordingly (see docs/design.md §6).
// +kubebuilder:validation:Enum=standard;gpu
type ChunkType string

const (
	// ChunkTypeStandard is a CPU / memory-only chunk.
	ChunkTypeStandard ChunkType = "standard"

	// ChunkTypeGPU is a chunk that includes at least one GPU.
	ChunkTypeGPU ChunkType = "gpu"
)

// ReservationPhase is the lifecycle phase of a Reservation, driven by the Broker.
//
// Happy path:
//
//	Pending → GeneratingKubeconfig → KubeconfigReady → Peering → Peered → Unpeering → Released
//
// Failure paths:
//
//	{Pending | GeneratingKubeconfig | KubeconfigReady | Peering} → Failed | Expired
//
// See docs/design.md §5.3 for the full state machine.
// +kubebuilder:validation:Enum=Pending;GeneratingKubeconfig;KubeconfigReady;Peering;Peered;Unpeering;Released;Expired;Failed
type ReservationPhase string

const (
	// ReservationPhasePending is the initial phase: the decision engine has
	// chosen a provider but nothing has been requested from it yet.
	ReservationPhasePending ReservationPhase = "Pending"

	// ReservationPhaseGeneratingKubeconfig means the Broker has queued a
	// ProviderInstruction{GenerateKubeconfig} for the provider agent and is
	// waiting for the resulting peering kubeconfig.
	ReservationPhaseGeneratingKubeconfig ReservationPhase = "GeneratingKubeconfig"

	// ReservationPhaseKubeconfigReady means the provider agent returned a peering
	// kubeconfig and the Broker has queued a ReservationInstruction{Peer} for the
	// consumer agent.
	ReservationPhaseKubeconfigReady ReservationPhase = "KubeconfigReady"

	// ReservationPhasePeering means the consumer agent is executing liqoctl peer
	// and creating the ResourceSlices for the reserved chunks.
	ReservationPhasePeering ReservationPhase = "Peering"

	// ReservationPhasePeered means Liqo has materialized virtual nodes for every
	// reserved chunk and the Reservation is actively serving the consumer.
	ReservationPhasePeered ReservationPhase = "Peered"

	// ReservationPhaseUnpeering means one or more chunks are being released; the
	// consumer agent is deleting ResourceSlices and, when lastChunk is true,
	// running liqoctl unpeer.
	ReservationPhaseUnpeering ReservationPhase = "Unpeering"

	// ReservationPhaseReleased is a terminal phase: every chunk has been released
	// and the Reservation is eligible for garbage collection.
	ReservationPhaseReleased ReservationPhase = "Released"

	// ReservationPhaseExpired is a terminal phase: the reservation-timeout
	// elapsed before the Reservation reached Peered.
	ReservationPhaseExpired ReservationPhase = "Expired"

	// ReservationPhaseFailed is a terminal phase: an irrecoverable error
	// occurred during creation, kubeconfig generation, or peering.
	ReservationPhaseFailed ReservationPhase = "Failed"
)

// Topology describes where a provider cluster is physically located.
// It is an optional input to the Broker's decision engine and is surfaced
// back to Cluster Autoscaler via node-group topology labels.
type Topology struct {
	// Zone is an availability-zone identifier.
	// Mirrors the value of the topology.kubernetes.io/zone node label.
	// +optional
	Zone string `json:"zone,omitempty"`

	// Region is a region identifier.
	// Mirrors the value of the topology.kubernetes.io/region node label.
	// +optional
	Region string `json:"region,omitempty"`
}
