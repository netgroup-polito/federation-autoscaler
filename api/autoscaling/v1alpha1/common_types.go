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

// This file holds types shared between more than one CRD in the
// autoscaling.federation-autoscaler.io group (VirtualNodeState and the two
// Instruction CRDs). Per-CRD types live in each CRD's own *_types.go file.

package v1alpha1

// VirtualNodeStatePhase is the lifecycle phase of a single virtual-node chunk
// on the consumer cluster.
//
// Happy path:   Creating → Running → Deleting → (object deleted)
// Failure path: Creating → Failed  → (object deleted during cleanup)
//
// See docs/design.md §5.1 for the full state machine.
// +kubebuilder:validation:Enum=Creating;Running;Deleting;Failed
type VirtualNodeStatePhase string

const (
	// VirtualNodeStatePhaseCreating means the gRPC server has requested the
	// chunk and the Consumer Agent is still peering / creating the
	// ResourceSlice; Liqo has not yet produced a virtual node.
	VirtualNodeStatePhaseCreating VirtualNodeStatePhase = "Creating"

	// VirtualNodeStatePhaseRunning means the corresponding Liqo virtual node
	// exists and is ready to schedule pods.
	VirtualNodeStatePhaseRunning VirtualNodeStatePhase = "Running"

	// VirtualNodeStatePhaseDeleting means the gRPC server has issued
	// NodeGroupDeleteNodes and the Consumer Agent is tearing the chunk down.
	VirtualNodeStatePhaseDeleting VirtualNodeStatePhase = "Deleting"

	// VirtualNodeStatePhaseFailed is a terminal phase: creation or tear-down
	// failed in a way the controller cannot retry automatically.
	VirtualNodeStatePhaseFailed VirtualNodeStatePhase = "Failed"
)

// ProviderInstructionKind enumerates the instruction types the Broker can
// queue for a Provider Agent (see docs/design.md §5.4 and §7.3.6).
// +kubebuilder:validation:Enum=GenerateKubeconfig;Cleanup;Reconcile
type ProviderInstructionKind string

const (
	// ProviderInstructionGenerateKubeconfig asks the Provider Agent to run
	// `liqoctl generate peering-user --consumer-cluster-id <id>` and return
	// the resulting kubeconfig via POST /api/v1/instructions/{id}/result.
	ProviderInstructionGenerateKubeconfig ProviderInstructionKind = "GenerateKubeconfig"

	// ProviderInstructionCleanup asks the Provider Agent to tear down
	// provider-side artefacts for a reservation whose last chunk has been
	// released (e.g. revoke the peering-user kubeconfig).
	ProviderInstructionCleanup ProviderInstructionKind = "Cleanup"

	// ProviderInstructionReconcile asks the Provider Agent to return a full
	// local snapshot so the Broker can diff-apply it after a restart or
	// leader change.
	ProviderInstructionReconcile ProviderInstructionKind = "Reconcile"
)

// ReservationInstructionKind enumerates the instruction types the Broker can
// queue for a Consumer Agent (see docs/design.md §5.5 and §7.3.6).
// +kubebuilder:validation:Enum=Peer;Unpeer;Cleanup;Reconcile
type ReservationInstructionKind string

const (
	// ReservationInstructionPeer asks the Consumer Agent to persist the
	// peering-user kubeconfig (carried inline in the polling response), run
	// liqoctl peer if not yet peered with the provider, create N
	// ResourceSlices, create or update the NamespaceOffloading, and return
	// the resulting virtual node names.
	ReservationInstructionPeer ReservationInstructionKind = "Peer"

	// ReservationInstructionUnpeer asks the Consumer Agent to delete the
	// specified ResourceSlices and, when lastChunk is true, run
	// liqoctl unpeer.
	ReservationInstructionUnpeer ReservationInstructionKind = "Unpeer"

	// ReservationInstructionCleanup asks the Consumer Agent to remove any
	// stale artefacts (orphan ResourceSlices, NamespaceOffloading entries,
	// or Liqo peerings) for a reservation that no longer exists on the
	// Broker side.
	ReservationInstructionCleanup ReservationInstructionKind = "Cleanup"

	// ReservationInstructionReconcile asks the Consumer Agent to return a
	// full local snapshot for the Broker to diff-apply.
	ReservationInstructionReconcile ReservationInstructionKind = "Reconcile"
)
