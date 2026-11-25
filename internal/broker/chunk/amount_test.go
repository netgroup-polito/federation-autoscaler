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

func rl(cpu, mem string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
}

func provider(reservedChunks int32, reserved corev1.ResourceList) *brokerv1alpha1.ClusterAdvertisement {
	cadv := &brokerv1alpha1.ClusterAdvertisement{}
	cadv.Spec.ClusterType = brokerv1alpha1.ChunkTypeStandard
	cadv.Spec.Resources.Allocatable = rl("8", "16Gi") // 4 standard chunks
	cadv.Status.ReservedChunks = reservedChunks
	cadv.Status.ReservedResources = reserved
	return cadv
}

func TestValidateAmount(t *testing.T) {
	cases := map[string]struct {
		amount corev1.ResourceList
		ok     bool
	}{
		"cpu and memory":  {rl("3", "6Gi"), true},
		"with a gpu":      {corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"), gpuResourceName: resource.MustParse("1")}, true},
		"memory missing":  {corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}, false},
		"zero cpu":        {rl("0", "1Gi"), false},
		"other resources": {corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"), corev1.ResourcePods: resource.MustParse("10")}, false},
	}
	for name, c := range cases {
		if err := ValidateAmount(c.amount); (err == nil) != c.ok {
			t.Errorf("%s: ValidateAmount err = %v, want ok=%v", name, err, c.ok)
		}
	}
}

func TestFreeForAmountAndTotalFor(t *testing.T) {
	sizer := NewDefaultSizer()
	cadv := provider(1, rl("3", "6Gi")) // one chunk (2 cpu / 4Gi) and one amount

	free := FreeForAmount(sizer, cadv)
	if got := free.Cpu().String(); got != "3" {
		t.Errorf("free cpu = %s, want 3", got)
	}
	if got := free.Memory().String(); got != "6Gi" {
		t.Errorf("free memory = %s, want 6Gi", got)
	}
	if !Fits(free, rl("3", "6Gi")) || Fits(free, rl("4", "1Gi")) {
		t.Errorf("Fits does not match the free capacity %s", Format(free))
	}
	if got := TotalFor(sizer, cadv); got != 2 { // 5 cpu / 10Gi left
		t.Errorf("TotalFor = %d, want 2", got)
	}
}

func TestApplyAmount(t *testing.T) {
	sizer := NewDefaultSizer()
	cadv := provider(1, nil)

	ApplyAmount(sizer, cadv, rl("3", "6Gi"), +1)
	if cadv.Status.TotalChunks != 2 || cadv.Status.AvailableChunks != 1 {
		t.Fatalf("after reserving: total=%d available=%d, want 2 and 1",
			cadv.Status.TotalChunks, cadv.Status.AvailableChunks)
	}

	ApplyAmount(sizer, cadv, rl("3", "6Gi"), -1)
	if len(cadv.Status.ReservedResources) != 0 {
		t.Errorf("after release: ReservedResources = %s, want empty", Format(cadv.Status.ReservedResources))
	}
	if cadv.Status.TotalChunks != 4 || cadv.Status.AvailableChunks != 3 {
		t.Errorf("after release: total=%d available=%d, want 4 and 3",
			cadv.Status.TotalChunks, cadv.Status.AvailableChunks)
	}

	// Releasing more than is reserved never goes below zero.
	ApplyAmount(sizer, cadv, rl("1", "1Gi"), -1)
	if len(cadv.Status.ReservedResources) != 0 {
		t.Errorf("over-release: ReservedResources = %s, want empty", Format(cadv.Status.ReservedResources))
	}
}
