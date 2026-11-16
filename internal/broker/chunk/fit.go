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
	corev1 "k8s.io/api/core/v1"
)

// GPUResourceName is the resource that makes a request, or a chunk, GPU-typed.
const GPUResourceName = gpuResourceName

// CountFor rounds a request UP to whole chunks of size perChunk: for each
// requested resource, ceil(requested / per-chunk), taking the max across
// resources. It returns 0 when a requested resource is absent from the chunk
// (chunks of that shape cannot hold the request) or when nothing is requested.
func CountFor(req, perChunk corev1.ResourceList) int32 {
	var n int64
	for name, q := range req {
		rv := q.MilliValue()
		if rv <= 0 {
			continue
		}
		cq, ok := perChunk[name]
		cv := cq.MilliValue()
		if !ok || cv <= 0 {
			return 0
		}
		if need := (rv + cv - 1) / cv; need > n {
			n = need
		}
	}
	if n < 1 {
		return 0
	}
	return int32(n)
}

// WantsGPU reports whether the request asks for at least one GPU.
func WantsGPU(req corev1.ResourceList) bool {
	q, ok := req[gpuResourceName]
	return ok && q.MilliValue() > 0
}
