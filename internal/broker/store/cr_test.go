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

package store

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/runtime"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// crTestNamespace is where these specs let the store write its
// advertisements. The fake client does not require it to exist.
const crTestNamespace = "federation-autoscaler-system"

var _ = Describe("CRStore", func() {
	// The same contract the in-memory store satisfies. Two very different
	// implementations passing one suite is what makes them interchangeable,
	// and a Redis store will call this same function.
	//
	// These specs use the fake client rather than envtest, so they need no
	// binaries and run in milliseconds. The trade-off is that the fake client
	// does not enforce the CRD's own validation, so it cannot prove a real API
	// server would accept every object; the existing envtest suites cover
	// that.
	RunOfferStoreConformance(func(freshFor time.Duration) OfferStore {
		s := runtime.NewScheme()
		Expect(brokerv1alpha1.AddToScheme(s)).To(Succeed())

		// WithStatusSubresource matters: the store writes the spec and the
		// status in two separate calls, as a real API server requires.
		// Without it the fake client treats status as part of a normal
		// update, and the spec write would clobber the counters the store
		// deliberately read back and left alone.
		c := clientfake.NewClientBuilder().
			WithScheme(s).
			WithStatusSubresource(&brokerv1alpha1.ClusterAdvertisement{}).
			Build()

		return NewCRStore(c, crTestNamespace, freshFor)
	})
})
