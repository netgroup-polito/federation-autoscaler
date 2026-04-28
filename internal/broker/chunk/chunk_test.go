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

package chunk

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// TestDefaultSizer covers the four canonical paths through DefaultSizer:
//   - balanced standard advertisement (cpu == memory in chunks)
//   - memory-constrained standard advertisement (memory caps the count)
//   - GPU advertisement (gpu count caps below cpu/memory)
//   - empty advertisement (no chunks at all)
func TestDefaultSizer(t *testing.T) {
	tests := []struct {
		name       string
		advertised corev1.ResourceList
		wantType   brokerv1alpha1.ChunkType
		wantCount  int32
	}{
		{
			name: "standard balanced",
			advertised: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("8"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
			},
			wantType:  brokerv1alpha1.ChunkTypeStandard,
			wantCount: 4,
		},
		{
			name: "standard memory-bound (leftover cpu discarded)",
			advertised: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("16"),
				corev1.ResourceMemory: resource.MustParse("12Gi"),
			},
			wantType:  brokerv1alpha1.ChunkTypeStandard,
			wantCount: 3, // mem caps to 3, cpu would have allowed 8
		},
		{
			name: "gpu count caps below cpu/memory",
			advertised: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("32"),
				corev1.ResourceMemory: resource.MustParse("64Gi"),
				gpuResourceName:       resource.MustParse("2"),
			},
			wantType:  brokerv1alpha1.ChunkTypeGPU,
			wantCount: 2,
		},
		{
			name:       "empty resources",
			advertised: corev1.ResourceList{},
			wantType:   brokerv1alpha1.ChunkTypeStandard,
			wantCount:  0,
		},
	}

	sizer := NewDefaultSizer()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sizer.Size(tt.advertised)
			if len(got) != 1 {
				t.Fatalf("DefaultSizer must return exactly one Result, got %d", len(got))
			}
			if got[0].Type != tt.wantType {
				t.Errorf("Type: got %q, want %q", got[0].Type, tt.wantType)
			}
			if got[0].Count != tt.wantCount {
				t.Errorf("Count: got %d, want %d", got[0].Count, tt.wantCount)
			}
		})
	}
}

// TestDefaultSizerPerChunkShape verifies the per-chunk resource shape so
// downstream node templates show stable, predictable values.
func TestDefaultSizerPerChunkShape(t *testing.T) {
	sizer := NewDefaultSizer()
	std := sizer.Size(corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
	})[0]
	if got := std.PerChunk[corev1.ResourceCPU]; got.Value() != 2 {
		t.Errorf("standard cpu: got %s, want 2", got.String())
	}
	if got := std.PerChunk[corev1.ResourceMemory]; got.Value() != 4*1024*1024*1024 {
		t.Errorf("standard memory: got %s, want 4Gi", got.String())
	}

	gpu := sizer.Size(corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("16"),
		corev1.ResourceMemory: resource.MustParse("32Gi"),
		gpuResourceName:       resource.MustParse("1"),
	})[0]
	if got := gpu.PerChunk[gpuResourceName]; got.Value() != 1 {
		t.Errorf("gpu nvidia.com/gpu: got %s, want 1", got.String())
	}
}
