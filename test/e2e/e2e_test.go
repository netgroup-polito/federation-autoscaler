//go:build e2e
// +build e2e

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

package e2e

import (
	"context"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/netgroup-polito/federation-autoscaler/test/e2e/kind"
	"github.com/netgroup-polito/federation-autoscaler/test/e2e/scenario"
)

// scaleUpTimeout caps the full happy-path: synthetic workload →
// Reservation Peered → VirtualNode Ready → Pods Running. 10 m is the
// upper bound on a healthy host; CI runners with slow image pulls may
// approach but not exceed it.
const scaleUpTimeout = 10 * time.Minute

var _ = Describe("happy-path federation scale-up across 4 Kind clusters", Ordered, func() {
	var result *scenario.HappyPathResult

	It("scales up the synthetic workload by carving a Reservation out of the federation", func() {
		ctx, cancel := context.WithTimeout(context.Background(), scaleUpTimeout)
		defer cancel()

		var err error
		result, err = scenario.RunHappyPath(ctx, scenario.HappyPathOptions{
			KubeconfigCentral:  topology.Kubeconfig(kind.RoleCentral),
			KubeconfigConsumer: topology.Kubeconfig(kind.RoleConsumer1),
			WorkloadNamespace:  "default",
			Replicas:           2,
			WaitOptions: scenario.WaitOptions{
				Timeout:  scaleUpTimeout / 3,
				Interval: 5 * time.Second,
			},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.ReservationName).NotTo(BeEmpty())
		Expect(result.VirtualNodeName).NotTo(BeEmpty())
	})

	It("releases the Liqo ResourceSlice + peering when the Reservation is deleted", func() {
		Expect(result).NotTo(BeNil(),
			"prior scale-up scenario must have populated result")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		By("deleting the Reservation on the central cluster")
		Expect(kubectlDelete(ctx,
			topology.Kubeconfig(kind.RoleCentral),
			"federation-autoscaler-system",
			"reservations.broker.federation-autoscaler.io/"+result.ReservationName,
		)).To(Succeed())

		By("waiting for the Liqo ResourceSlice on the consumer to disappear")
		Eventually(func(g Gomega) {
			out, err := kubectlGet(ctx,
				topology.Kubeconfig(kind.RoleConsumer1),
				"federation-autoscaler-system",
				"resourceslices.authentication.liqo.io",
				"-l", "federation-autoscaler.io/reservation="+result.ReservationName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(SatisfyAny(
				ContainSubstring("No resources found"),
				BeEmpty(),
			), "ResourceSlice still present: %s", out)
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for the Liqo VirtualNode to disappear")
		Eventually(func(g Gomega) {
			out, err := kubectlGet(ctx,
				topology.Kubeconfig(kind.RoleConsumer1),
				"federation-autoscaler-system",
				"virtualnodes.offloading.liqo.io",
				"-l", "federation-autoscaler.io/reservation="+result.ReservationName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(SatisfyAny(
				ContainSubstring("No resources found"),
				BeEmpty(),
			), "VirtualNode still present: %s", out)
		}, 3*time.Minute, 5*time.Second).Should(Succeed())
	})
})

// kubectlGet runs `kubectl get` against the named kubeconfig and
// returns its stdout. The federation-autoscaler suite uses kubectl
// shell-outs across the board for consistency with the bootstrap /
// scenario packages.
func kubectlGet(ctx context.Context, kubeconfig, namespace, resource string, extra ...string) (string, error) {
	args := append([]string{
		"--kubeconfig=" + kubeconfig,
		"get", resource,
		"--namespace", namespace,
		"--no-headers",
	}, extra...)
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// kubectlDelete deletes the named resource. Ignore-not-found is set so
// re-runs (or AfterEach guards) stay idempotent.
func kubectlDelete(ctx context.Context, kubeconfig, namespace, resource string) error {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig="+kubeconfig,
		"delete", resource,
		"--namespace", namespace,
		"--ignore-not-found")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &kubectlError{output: string(out), err: err}
	}
	return nil
}

type kubectlError struct {
	output string
	err    error
}

func (e *kubectlError) Error() string {
	return strings.TrimSpace(e.output)
}

func (e *kubectlError) Unwrap() error { return e.err }
