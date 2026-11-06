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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// The CRStore specs in cr_test.go run against the fake client: fast, no
// binaries, and enough for the logic. What they cannot prove is that a real
// API server behaves the way the fake one does — above all that it rejects a
// write whose resource version is stale, which is the only thing standing
// between two consumers and the same chunk.
//
// So the same suite runs a second time here against envtest, a real etcd and
// API server started for the test. If the binaries are not installed the
// specs skip rather than fail, because `go test` on a fresh clone should not
// go red over a missing download. `make setup-envtest` installs them.

var (
	envtestEnv    *envtest.Environment
	envtestClient client.Client
	envtestReason string

	// envtestNS numbers a fresh namespace per spec. A real API server keeps
	// what previous specs wrote, and every rule assumes an empty catalog.
	envtestNS atomic.Int32
)

var _ = BeforeSuite(func() {
	dir := envtestBinaryDir()
	if dir == "" {
		envtestReason = "envtest binaries not found under bin/k8s — run: make setup-envtest"
		return
	}

	Expect(brokerv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())

	envtestEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: dir,
	}

	cfg, err := envtestEnv.Start()
	Expect(err).NotTo(HaveOccurred())

	envtestClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	if envtestEnv != nil {
		Expect(envtestEnv.Stop()).To(Succeed())
	}
})

var _ = Describe("CRStore on a real API server", func() {
	RunOfferStoreConformance(func(freshFor time.Duration) OfferStore {
		if envtestClient == nil {
			Skip(envtestReason)
		}

		ns := fmt.Sprintf("conformance-%d", envtestNS.Add(1))
		Expect(envtestClient.Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())

		return NewCRStore(envtestClient, ns, freshFor)
	})
})

// envtestBinaryDir mirrors the helper the controller suites use, so the
// assets are found when running from the IDE as well as through make.
func envtestBinaryDir() string {
	base := filepath.Join("..", "..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}
