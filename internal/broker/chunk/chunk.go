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

// Package chunk implements the Broker's "divide a provider's advertisement
// into fixed-size chunks" policy (docs/design.md §6). Each chunk surfaces to
// the Cluster Autoscaler as one slot in a node group; consumers reserve
// chunks rather than raw cpu/memory.
//
// Step 4f wires this package via a small Sizer interface so the future
// ConfigMap-backed implementation (step 4h) can drop in without touching
// the HTTP handlers.
package chunk

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// Result is what Sizer.Size returns for one classified advertisement.
type Result struct {
	// Type is the inferred ChunkType for this set of resources.
	Type brokerv1alpha1.ChunkType

	// Count is how many full chunks fit in the advertised resources. The
	// remainder is discarded (Option B from the design).
	Count int32

	// PerChunk is one chunk's worth of cpu/memory/(gpu).
	PerChunk corev1.ResourceList
}

// Sizer is the policy boundary between the HTTP layer and chunk maths.
// Implementations MUST be safe for concurrent use because the HTTP server
// invokes them from many goroutines simultaneously.
type Sizer interface {
	// Size classifies advertised and returns 0..1 chunk groups. The default
	// implementation returns one Result; a future ConfigMap-driven sizer may
	// produce both standard and gpu groups from a single advertisement.
	Size(advertised corev1.ResourceList) []Result
}

// DefaultSizer hard-codes the design's fallback chunk sizes (docs/design.md
// §6.2): standard chunks are cpu=2, memory=4Gi; gpu chunks add 1 GPU and a
// larger cpu/memory box. ConfigMap-driven sizing replaces this in step 4h
// without changing Sizer's signature.
type DefaultSizer struct{}

// NewDefaultSizer returns a ready-to-use DefaultSizer.
func NewDefaultSizer() *DefaultSizer { return &DefaultSizer{} }

// gpuResourceName is the conventional Kubernetes name for NVIDIA GPUs and
// the only one DefaultSizer recognises. Future sizers may accept arbitrary
// extended resources via the ConfigMap.
const gpuResourceName corev1.ResourceName = "nvidia.com/gpu"

// Size implements Sizer: one Result whose Type depends on whether the
// advertisement reports a non-zero GPU count.
func (DefaultSizer) Size(advertised corev1.ResourceList) []Result {
	gpuQty, hasGPU := advertised[gpuResourceName]
	gpuCount := gpuQty.Value()

	if hasGPU && gpuCount > 0 {
		return []Result{sizeGPU(advertised, gpuCount)}
	}
	return []Result{sizeStandard(advertised)}
}

// sizeStandard divides cpu/memory into 2-cpu / 4-Gi chunks. Floor division
// is intentional: leftover capacity (e.g. 1 cpu spare) is discarded so every
// chunk carries an identical, schedulable shape.
func sizeStandard(advertised corev1.ResourceList) Result {
	const (
		stdCPUMilli int64 = 2 * 1000               // 2 cores
		stdMemBytes int64 = 4 * 1024 * 1024 * 1024 // 4 GiB
	)
	cpuQty := advertised[corev1.ResourceCPU]
	memQty := advertised[corev1.ResourceMemory]

	cpuChunks := cpuQty.MilliValue() / stdCPUMilli
	memChunks := memQty.Value() / stdMemBytes

	count := int32(min64(cpuChunks, memChunks))
	if count < 0 {
		count = 0
	}

	return Result{
		Type:  brokerv1alpha1.ChunkTypeStandard,
		Count: count,
		PerChunk: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(stdMemBytes, resource.BinarySI),
		},
	}
}

// sizeGPU dedicates one chunk per advertised GPU and pairs it with a 4-cpu
// / 8-Gi box (workloads that pin a GPU usually want generous host
// resources). cpu/memory shortfalls cap the chunk count below the GPU
// count.
func sizeGPU(advertised corev1.ResourceList, gpuCount int64) Result {
	const (
		gpuCPUMilli int64 = 4 * 1000               // 4 cores
		gpuMemBytes int64 = 8 * 1024 * 1024 * 1024 // 8 GiB
	)
	cpuQty := advertised[corev1.ResourceCPU]
	memQty := advertised[corev1.ResourceMemory]

	cpuChunks := cpuQty.MilliValue() / gpuCPUMilli
	memChunks := memQty.Value() / gpuMemBytes

	count := int32(min64(min64(cpuChunks, memChunks), gpuCount))
	if count < 0 {
		count = 0
	}

	return Result{
		Type:  brokerv1alpha1.ChunkTypeGPU,
		Count: count,
		PerChunk: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(gpuMemBytes, resource.BinarySI),
			gpuResourceName:       *resource.NewQuantity(1, resource.DecimalSI),
		},
	}
}

// min64 — Go's stdlib min only works on numeric types in 1.21+ and is a
// generic; using a local helper keeps the file friendly to older toolchains
// the related-codebases pin.
func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
