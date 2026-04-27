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

// Package api defines the HTTP wire types of the Broker's REST surface
// (docs/design.md §7.3). Every endpoint a Provider or Consumer Agent calls
// over mTLS goes through one of these structs. Type-level field comments
// reference the design-document section they implement.
//
// The package deliberately keeps the HTTP/JSON model decoupled from the
// CRD types in api/broker/v1alpha1 and api/autoscaling/v1alpha1: it imports
// only the small enums and value structs (ChunkType, ReservationPhase,
// Topology, the instruction Kind enums) so the wire schema can evolve
// without forcing CRD changes (and vice-versa).
package api

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// -----------------------------------------------------------------------------
// HTTP-layer constants (docs/design.md §7.0)
// -----------------------------------------------------------------------------

// HTTP header names used by the Broker REST surface.
const (
	// HeaderRequestID carries an optional client-generated correlation ID.
	// Servers MUST echo it in the response and in their logs.
	HeaderRequestID = "X-Request-Id"

	// HeaderReservationID carries the idempotency key for reservation-scoped
	// mutating calls (POST /api/v1/reservations and POST
	// /api/v1/instructions/{id}/result). Re-submissions with the same value
	// MUST return the original response within idempotency-cache-ttl.
	HeaderReservationID = "X-Reservation-Id"
)

// ContentTypeJSON is the only Content-Type the Broker accepts or emits.
const ContentTypeJSON = "application/json; charset=utf-8"

// ErrorCode is the value of ErrorResponse.Code; clients switch on it instead
// of HTTP status to handle stable, machine-readable error categories
// (docs/design.md §7.0).
type ErrorCode string

const (
	ErrCodeInvalidRequest       ErrorCode = "InvalidRequest"
	ErrCodeUnauthenticated      ErrorCode = "Unauthenticated"
	ErrCodeForbidden            ErrorCode = "Forbidden"
	ErrCodeNotFound             ErrorCode = "NotFound"
	ErrCodeConflict             ErrorCode = "Conflict"
	ErrCodeReservationExpired   ErrorCode = "ReservationExpired"
	ErrCodeInsufficientCapacity ErrorCode = "InsufficientCapacity"
	ErrCodeTooManyRequests      ErrorCode = "TooManyRequests"
	ErrCodeInternalError        ErrorCode = "InternalError"
	ErrCodeUpstreamError        ErrorCode = "UpstreamError"
	ErrCodeServiceUnavailable   ErrorCode = "ServiceUnavailable"
	ErrCodeTimeout              ErrorCode = "Timeout"
)

// ErrorResponse is the body returned with every 4xx/5xx response.
type ErrorResponse struct {
	Code      ErrorCode      `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details,omitempty"`
	RequestID string         `json:"requestId,omitempty"`
}

// ErrorEnvelope is the shape used inside InstructionResultRequest.Error when
// reporting a Failed outcome. Same fields as ErrorResponse minus RequestID.
type ErrorEnvelope struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// -----------------------------------------------------------------------------
// 7.3.1 — POST /api/v1/advertisements   (Provider Agent → Broker, every 30 s)
// -----------------------------------------------------------------------------

// AdvertisementRequest is the body of POST /api/v1/advertisements. The same
// shape is also embedded inside ReconcilePayload.Advertisement so a Reconcile
// instruction can return the agent's authoritative truth.
type AdvertisementRequest struct {
	// ClusterID MUST equal the CN of the agent's mTLS client certificate.
	ClusterID string `json:"clusterId"`

	// LiqoClusterID is the agent's Liqo cluster identifier, used by the
	// Consumer Agent's `liqoctl peer` invocation.
	LiqoClusterID string `json:"liqoClusterId"`

	// Resources is the allocatable capacity the provider is donating right
	// now (cpu, memory, nvidia.com/gpu, …).
	Resources corev1.ResourceList `json:"resources"`

	// Topology is optional zone/region metadata used by the decision engine
	// and surfaced as Liqo virtual-node labels.
	Topology *brokerv1alpha1.Topology `json:"topology,omitempty"`

	// Price is the cost per chunk-hour. Float-or-string form is supported by
	// resource.Quantity's UnmarshalJSON.
	Price *resource.Quantity `json:"price,omitempty"`

	// LiqoLabels are stamped on the virtual nodes Liqo creates on each
	// peering consumer, e.g. liqo.io/type=virtual-node.
	LiqoLabels map[string]string `json:"liqoLabels,omitempty"`

	// LiqoTaints are applied to virtual nodes alongside LiqoLabels.
	LiqoTaints []corev1.Taint `json:"liqoTaints,omitempty"`
}

// AdvertisementResponse is returned by POST /api/v1/advertisements. It
// piggy-backs zero or more ProviderInstructions so a 30 s heartbeat can
// also deliver work, halving worst-case instruction latency.
type AdvertisementResponse struct {
	// Accepted is false only when the Broker silently dropped the
	// advertisement (e.g. cluster ID is on a deny list). Errors otherwise
	// surface as a non-2xx status code with an ErrorResponse body.
	Accepted bool `json:"accepted"`

	// ChunkCount is the number of chunks the Broker derived from Resources
	// using the chunk-config ConfigMap.
	ChunkCount int32 `json:"chunkCount"`

	// ChunkResources is the per-chunk capacity (one chunk's worth, not
	// ChunkCount × chunk).
	ChunkResources corev1.ResourceList `json:"chunkResources"`

	// NextReportIn hints the agent at the next advertisement deadline.
	// Format follows time.Duration's String(), e.g. "30s".
	NextReportIn string `json:"nextReportIn"`

	// Instructions is the same payload GET /api/v1/instructions returns for
	// this provider; see InstructionView for the schema.
	Instructions []InstructionView `json:"instructions,omitempty"`
}

// -----------------------------------------------------------------------------
// 7.3.2 — GET /api/v1/advertisements/{clusterId}   (Provider Agent → Broker)
// -----------------------------------------------------------------------------

// AdvertisementSnapshot is the Broker's authoritative view of one provider's
// advertisement. Returned by GET /api/v1/advertisements/{clusterId} so the
// Provider Agent can preserve the Broker-managed Reserved field across
// re-submissions (same convention as upstream k8s-resource-brokering).
type AdvertisementSnapshot struct {
	ClusterID       string                   `json:"clusterId"`
	LiqoClusterID   string                   `json:"liqoClusterId"`
	Resources       corev1.ResourceList      `json:"resources"`
	Topology        *brokerv1alpha1.Topology `json:"topology,omitempty"`
	Price           *resource.Quantity       `json:"price,omitempty"`
	LiqoLabels      map[string]string        `json:"liqoLabels,omitempty"`
	LiqoTaints      []corev1.Taint           `json:"liqoTaints,omitempty"`
	ChunkCount      int32                    `json:"chunkCount"`
	ReservedChunks  int32                    `json:"reservedChunks"`
	AvailableChunks int32                    `json:"availableChunks"`
	LastSeen        metav1.Time              `json:"lastSeen"`
	Available       bool                     `json:"available"`
}

// -----------------------------------------------------------------------------
// 7.3.3 — POST /api/v1/heartbeat   (Consumer Agent → Broker, every 15 s)
// -----------------------------------------------------------------------------

type HeartbeatRequest struct {
	ClusterID     string `json:"clusterId"`
	LiqoClusterID string `json:"liqoClusterId"`
}

type HeartbeatResponse struct {
	AckAt metav1.Time `json:"ackAt"`
}

// -----------------------------------------------------------------------------
// 7.3.4 — GET /api/v1/nodegroups   (Consumer Agent → Broker)
// -----------------------------------------------------------------------------

// NodeGroupView is the wire-level representation of a node group; one entry
// per (provider × chunk type) pair. Identical schema is reused by the
// Consumer Agent on its local /local/nodegroups endpoint.
type NodeGroupView struct {
	ID                    string                   `json:"id"`
	ProviderClusterID     string                   `json:"providerClusterId"`
	ProviderLiqoClusterID string                   `json:"providerLiqoClusterId"`
	Type                  brokerv1alpha1.ChunkType `json:"type"`
	MinSize               int32                    `json:"minSize"`
	MaxSize               int32                    `json:"maxSize"`
	CurrentReserved       int32                    `json:"currentReserved"`
	ChunkResources        corev1.ResourceList      `json:"chunkResources"`
	Cost                  *resource.Quantity       `json:"cost,omitempty"`
	Topology              *brokerv1alpha1.Topology `json:"topology,omitempty"`
	Labels                map[string]string        `json:"labels,omitempty"`
	Taints                []corev1.Taint           `json:"taints,omitempty"`
}

type NodeGroupListResponse struct {
	NodeGroups      []NodeGroupView `json:"nodeGroups"`
	Generation      int64           `json:"generation"`
	ServedAt        metav1.Time     `json:"servedAt"`
	CacheAgeSeconds int32           `json:"cacheAgeSeconds"`
}

// -----------------------------------------------------------------------------
// 7.3.5 — POST /api/v1/reservations   (Consumer Agent → Broker, synchronous)
// -----------------------------------------------------------------------------

// ReservationRequest is the body of POST /api/v1/reservations. Idempotency
// key carried in the X-Reservation-Id header.
type ReservationRequest struct {
	ProviderClusterID string                   `json:"providerClusterId"`
	NodeGroupID       string                   `json:"nodeGroupId"`
	ChunkCount        int32                    `json:"chunkCount"`
	ChunkType         brokerv1alpha1.ChunkType `json:"chunkType"`
	Namespaces        []string                 `json:"namespaces,omitempty"`
}

// ReservationResponse is returned by both POST /api/v1/reservations
// (HTTP 201) and GET /api/v1/reservations/{id} (HTTP 200). Some optional
// fields are populated only after the asynchronous peering steps complete.
type ReservationResponse struct {
	ReservationID         string                          `json:"reservationId"`
	Status                brokerv1alpha1.ReservationPhase `json:"status"`
	ProviderClusterID     string                          `json:"providerClusterId,omitempty"`
	ProviderLiqoClusterID string                          `json:"providerLiqoClusterId,omitempty"`
	ConsumerClusterID     string                          `json:"consumerClusterId,omitempty"`
	ConsumerLiqoClusterID string                          `json:"consumerLiqoClusterId,omitempty"`
	ChunkCount            int32                           `json:"chunkCount"`
	ChunkType             brokerv1alpha1.ChunkType        `json:"chunkType,omitempty"`
	Resources             corev1.ResourceList             `json:"resources,omitempty"`
	VirtualNodeNames      []string                        `json:"virtualNodeNames,omitempty"`
	CreatedAt             metav1.Time                     `json:"createdAt"`
	ActivatedAt           *metav1.Time                    `json:"activatedAt,omitempty"`
	ExpiresAt             *metav1.Time                    `json:"expiresAt,omitempty"`
	Message               string                          `json:"message,omitempty"`
}

// -----------------------------------------------------------------------------
// 7.3.6 — GET /api/v1/instructions   (Both agents → Broker, every 5 s)
// -----------------------------------------------------------------------------

// InstructionView is the polymorphic wire representation of either a
// ProviderInstruction or a ReservationInstruction. The Broker selects which
// flavour to emit based on the caller's certificate role; the agent
// dispatches on the Kind field.
type InstructionView struct {
	// Common metadata.
	ID            string `json:"id"`
	Kind          string `json:"kind"` // ProviderInstructionKind ∪ ReservationInstructionKind
	ReservationID string `json:"reservationId"`
	ChunkCount    int32  `json:"chunkCount,omitempty"`
	LastChunk     bool   `json:"lastChunk,omitempty"`

	// Set on ProviderInstructions (kind ∈ GenerateKubeconfig|Cleanup|Reconcile).
	ConsumerClusterID     string `json:"consumerClusterId,omitempty"`
	ConsumerLiqoClusterID string `json:"consumerLiqoClusterId,omitempty"`

	// Set on ReservationInstructions (kind ∈ Peer|Unpeer|Cleanup|Reconcile).
	ProviderClusterID      string              `json:"providerClusterId,omitempty"`
	ProviderLiqoClusterID  string              `json:"providerLiqoClusterId,omitempty"`
	Kubeconfig             string              `json:"kubeconfig,omitempty"` // base64-encoded PEM, populated for Peer
	ResourceSliceResources corev1.ResourceList `json:"resourceSliceResources,omitempty"`
	ResourceSliceNames     []string            `json:"resourceSliceNames,omitempty"` // populated for Unpeer
	Namespaces             []string            `json:"namespaces,omitempty"`

	// Issued / Expires bracket the validity window. The Broker re-emits an
	// instruction on every poll until the agent reports a result (Enforced).
	IssuedAt  metav1.Time `json:"issuedAt"`
	ExpiresAt metav1.Time `json:"expiresAt"`
}

type InstructionsResponse struct {
	Instructions []InstructionView `json:"instructions"`
}

// -----------------------------------------------------------------------------
// 7.3.7 — POST /api/v1/instructions/{id}/result   (Both agents → Broker)
// -----------------------------------------------------------------------------

// ResultStatus signals success or failure of the instruction. Anything else
// MUST be rejected with HTTP 400.
type ResultStatus string

const (
	ResultStatusSucceeded ResultStatus = "Succeeded"
	ResultStatusFailed    ResultStatus = "Failed"
)

// PayloadKind discriminates the union shape inside ResultPayload. Required
// when Status==Succeeded and the instruction kind has a non-empty payload.
type PayloadKind string

const (
	PayloadKindKubeconfig PayloadKind = "KubeconfigPayload"
	PayloadKindPeer       PayloadKind = "PeerPayload"
	PayloadKindUnpeer     PayloadKind = "UnpeerPayload"
	PayloadKindReconcile  PayloadKind = "ReconcilePayload"
)

// ResultPayload is a tagged union; only the fields matching Kind are read by
// the Broker. Unrecognised fields are ignored to allow forward-compatible
// extensions.
type ResultPayload struct {
	Kind PayloadKind `json:"kind"`

	// KubeconfigPayload (Provider GenerateKubeconfig).
	Kubeconfig string       `json:"kubeconfig,omitempty"`
	ExpiresAt  *metav1.Time `json:"expiresAt,omitempty"`

	// PeerPayload (Consumer Peer).
	VirtualNodeNames   []string `json:"virtualNodeNames,omitempty"`
	ResourceSliceNames []string `json:"resourceSliceNames,omitempty"`

	// UnpeerPayload (Consumer Unpeer).
	ReleasedChunks int32 `json:"releasedChunks,omitempty"`
	TunnelDropped  bool  `json:"tunnelDropped,omitempty"`

	// ReconcilePayload (Both Reconcile). Mutually-exclusive with each other:
	// providers populate Advertisement, consumers populate VirtualNodeStates.
	Advertisement     *AdvertisementRequest           `json:"advertisement,omitempty"`
	VirtualNodeStates []ReconcileVirtualNodeStateView `json:"virtualNodeStates,omitempty"`
}

// ReconcileVirtualNodeStateView is the consumer-side condensed view returned
// inside ReconcilePayload.VirtualNodeStates. Mirrors a subset of
// autoscaling.federation-autoscaler.io/VirtualNodeState.spec+status that is
// enough for the Broker to detect drift.
type ReconcileVirtualNodeStateView struct {
	ReservationID   string                                    `json:"reservationId"`
	ChunkIndex      int32                                     `json:"chunkIndex"`
	NodeGroupID     string                                    `json:"nodeGroupId"`
	VirtualNodeName string                                    `json:"virtualNodeName,omitempty"`
	ResourceSlice   string                                    `json:"resourceSliceName,omitempty"`
	Phase           autoscalingv1alpha1.VirtualNodeStatePhase `json:"phase"`
	LastTransition  *metav1.Time                              `json:"lastTransitionTime,omitempty"`
}

type InstructionResultRequest struct {
	Status  ResultStatus   `json:"status"`
	Payload *ResultPayload `json:"payload,omitempty"`
	Error   *ErrorEnvelope `json:"error,omitempty"`
}

type InstructionResultResponse struct {
	Accepted bool `json:"accepted"`
}

// -----------------------------------------------------------------------------
// 7.3.8 — GET /api/v1/reservations/{id}   (Both agents → Broker)
// -----------------------------------------------------------------------------

// (No dedicated request struct: id is in the path. Response reuses
// ReservationResponse defined under §7.3.5.)

// -----------------------------------------------------------------------------
// 7.3.9 — DELETE /api/v1/reservations/{id}   (Consumer Agent → Broker)
// -----------------------------------------------------------------------------

// ReleaseRequest captures the optional `?chunks=N` query parameter when
// servers want to thread it through the type system. The HTTP layer parses
// it directly from the URL; this struct is provided for symmetry with the
// other endpoints and for unit tests.
type ReleaseRequest struct {
	// Chunks is the number of chunks to release; omit (zero) to release all.
	Chunks int32 `json:"chunks,omitempty"`
}

type ReleaseResponse struct {
	ReservationID       string                          `json:"reservationId"`
	Status              brokerv1alpha1.ReservationPhase `json:"status"`
	RemainingChunkCount int32                           `json:"remainingChunkCount"`
}
