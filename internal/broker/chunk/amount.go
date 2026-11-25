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
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// Amount reservations lend any CPU and memory amount as one virtual node,
// next to the fixed-size chunks. The provider's capacity is shared:
//
//   - amounts are summed in Status.ReservedResources;
//   - TotalChunks is computed from the advertised capacity minus that sum, so
//     a chunk never uses capacity an amount already holds;
//   - an amount fits only in what is left after the reserved chunks and the
//     other amounts.

// amountResources are the resources an amount may ask for.
var amountResources = map[corev1.ResourceName]bool{
	corev1.ResourceCPU:    true,
	corev1.ResourceMemory: true,
	gpuResourceName:       true,
}

// ValidateAmount checks a requested amount: CPU and memory are required, every
// value is positive, and only cpu, memory and nvidia.com/gpu may be asked for.
func ValidateAmount(amount corev1.ResourceList) error {
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		if _, ok := amount[name]; !ok {
			return fmt.Errorf("resources.%s is required", name)
		}
	}
	for name, q := range amount {
		if !amountResources[name] {
			return fmt.Errorf("resources.%s is not supported (only cpu, memory, %s)", name, gpuResourceName)
		}
		if q.Sign() <= 0 {
			return fmt.Errorf("resources.%s must be positive, got %s", name, q.String())
		}
	}
	return nil
}

// PerChunkFor returns one chunk's resources for the provider's chunk type, or
// an empty list when the sizer produces nothing for that type.
func PerChunkFor(sizer Sizer, cadv *brokerv1alpha1.ClusterAdvertisement) corev1.ResourceList {
	results := sizer.Size(cadv.Spec.Resources.Allocatable)
	for _, r := range results {
		if r.Type == cadv.Spec.ClusterType {
			return r.PerChunk
		}
	}
	if len(results) > 0 {
		return results[0].PerChunk
	}
	return corev1.ResourceList{}
}

// TotalFor returns how many chunks of the provider's type fit in its advertised
// capacity minus the amounts already reserved (Status.ReservedResources).
func TotalFor(sizer Sizer, cadv *brokerv1alpha1.ClusterAdvertisement) int32 {
	left := subtract(cadv.Spec.Resources.Allocatable, cadv.Status.ReservedResources)
	for _, r := range sizer.Size(left) {
		if r.Type == cadv.Spec.ClusterType {
			return r.Count
		}
	}
	return 0
}

// FreeForAmount returns what an amount may still use on the provider: the
// advertised CPU, memory and GPUs minus the reserved chunks and the reserved
// amounts, never negative. Other advertised resources (pods, storage) are
// left out: an amount cannot ask for them.
func FreeForAmount(sizer Sizer, cadv *brokerv1alpha1.ClusterAdvertisement) corev1.ResourceList {
	used := corev1.ResourceList{}
	perChunk := PerChunkFor(sizer, cadv)
	for name, q := range perChunk {
		total := q.DeepCopy()
		total.Mul(int64(cadv.Status.ReservedChunks))
		used[name] = total
	}
	add(used, cadv.Status.ReservedResources)
	advertised := corev1.ResourceList{}
	for name, q := range cadv.Spec.Resources.Allocatable {
		if amountResources[name] {
			advertised[name] = q
		}
	}
	return subtract(advertised, used)
}

// Fits reports whether every resource of amount is within free.
func Fits(free, amount corev1.ResourceList) bool {
	for name, want := range amount {
		have, ok := free[name]
		if !ok || have.Cmp(want) < 0 {
			return false
		}
	}
	return true
}

// ApplyAmount adds (sign > 0) or removes (sign < 0) amount to the provider's
// Status.ReservedResources and recomputes TotalChunks and AvailableChunks. It
// changes cadv in memory only; the caller writes the status.
func ApplyAmount(sizer Sizer, cadv *brokerv1alpha1.ClusterAdvertisement, amount corev1.ResourceList, sign int) {
	reserved := cadv.Status.ReservedResources.DeepCopy()
	if reserved == nil {
		reserved = corev1.ResourceList{}
	}
	if sign >= 0 {
		add(reserved, amount)
	} else {
		reserved = subtract(reserved, amount)
	}
	for name, q := range reserved {
		if q.Sign() <= 0 {
			delete(reserved, name)
		}
	}
	if len(reserved) == 0 {
		reserved = nil
	}
	cadv.Status.ReservedResources = reserved
	cadv.Status.TotalChunks = TotalFor(sizer, cadv)
	cadv.Status.AvailableChunks = max(cadv.Status.TotalChunks-cadv.Status.ReservedChunks, 0)
}

// Format renders a resource list as "cpu=3, memory=6Gi" in a stable order, for
// error messages.
func Format(list corev1.ResourceList) string {
	names := make([]string, 0, len(list))
	for name := range list {
		names = append(names, string(name))
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		q := list[corev1.ResourceName(name)]
		parts = append(parts, name+"="+q.String())
	}
	return strings.Join(parts, ", ")
}

// add adds b into a, resource by resource.
func add(a, b corev1.ResourceList) {
	for name, q := range b {
		cur, ok := a[name]
		if !ok {
			a[name] = q.DeepCopy()
			continue
		}
		cur.Add(q)
		a[name] = cur
	}
}

// subtract returns a minus b for every resource of a, never below zero.
func subtract(a, b corev1.ResourceList) corev1.ResourceList {
	out := corev1.ResourceList{}
	for name, q := range a {
		left := q.DeepCopy()
		if sub, ok := b[name]; ok {
			left.Sub(sub)
		}
		if left.Sign() < 0 {
			left = *resource.NewQuantity(0, q.Format)
		}
		out[name] = left
	}
	return out
}
