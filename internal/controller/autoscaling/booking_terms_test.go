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

package autoscaling

import (
	"testing"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
)

func TestBookingTermsOf(t *testing.T) {
	rr := &autoscalingv1alpha1.ResourceRequest{}
	rr.Spec.Duration = "2h"
	rr.Spec.LatencyBound = &autoscalingv1alpha1.LatencyBound{MaxLatencyMs: 3}
	if got := bookingTermsOf(rr); got.durationSeconds != 7200 || got.maxLatencyMs != 3 {
		t.Fatalf("bookingTermsOf = %+v; want 7200 s and 3 ms", got)
	}

	rr = &autoscalingv1alpha1.ResourceRequest{}
	rr.Spec.Duration = "soon" // does not parse: the broker's default applies
	if got := bookingTermsOf(rr); got.durationSeconds != 0 || got.maxLatencyMs != 0 {
		t.Fatalf("bookingTermsOf = %+v; want nothing", got)
	}
}
