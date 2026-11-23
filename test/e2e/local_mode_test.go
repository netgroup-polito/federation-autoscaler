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
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/netgroup-polito/federation-autoscaler/test/e2e/bootstrap"
	"github.com/netgroup-polito/federation-autoscaler/test/e2e/kind"
	"github.com/netgroup-polito/federation-autoscaler/test/e2e/scenario"
)

// Local mode (M2): the consumer decides from the Redis catalog, and each
// provider writes its own offer there. Self-contained: it clears what the
// happy path may have left, switches the agents to local mode, and switches
// them back at the end, so the two Describes can run in either order.
var _ = Describe("local mode: the consumer decides from the Redis catalog the providers fill", Ordered, func() {
	var (
		central, consumer string
		providers         []string
		catalogAddr       string
		freeBefore        map[string]string // provider ID -> offer CPU before the scale-up
		result            *scenario.HappyPathResult
	)

	BeforeAll(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		central = topology.Kubeconfig(kind.RoleCentral)
		consumer = topology.Kubeconfig(kind.RoleConsumer1)
		providers = []string{topology.Kubeconfig(kind.RoleProvider1), topology.Kubeconfig(kind.RoleProvider2)}
		catalogAddr = fmt.Sprintf("%s-control-plane:30379", kind.ClusterName(kind.RoleCentral))

		By("clearing any workload and Reservation left by an earlier spec")
		Expect(kubectlRun(ctx, consumer, "delete", "deployment", scenario.WorkloadName,
			"--namespace", "default", "--ignore-not-found")).To(Succeed())
		Expect(kubectlRun(ctx, central, "delete", "reservations.broker.federation-autoscaler.io", "--all",
			"--namespace", bootstrap.BrokerNamespace)).To(Succeed())
		Eventually(func(g Gomega) {
			out, err := kubectlGet(ctx, central, bootstrap.BrokerNamespace, "reservations.broker.federation-autoscaler.io")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(SatisfyAny(BeEmpty(), ContainSubstring("No resources found")))
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("[central] deploying the Redis catalog")
		Expect(bootstrap.ApplyOverlay(ctx, bootstrap.ApplyOverlayOptions{
			Kubeconfig:  central,
			OverlayPath: filepath.Join(repoRoot(), "config", "catalog-redis"),
		})).To(Succeed())
		Expect(kubectlRun(ctx, central, "rollout", "status", "deployment/catalog-redis",
			"--namespace", bootstrap.BrokerNamespace, "--timeout", "5m")).To(Succeed())

		for i, p := range providers {
			By(fmt.Sprintf("[provider-%d] writing its own offer to %s", i+1, catalogAddr))
			Expect(patchAgentConfig(ctx, p, map[string]string{"catalogAddr": catalogAddr})).To(Succeed())
			Expect(bootstrap.RolloutRestart(ctx, p, bootstrap.BrokerNamespace, "agent")).To(Succeed())
		}
		By("[consumer-1] deciding locally from the catalog")
		Expect(patchAgentConfig(ctx, consumer,
			map[string]string{"brainMode": "local", "catalogAddr": catalogAddr})).To(Succeed())
		Expect(bootstrap.RolloutRestart(ctx, consumer, bootstrap.BrokerNamespace, "agent")).To(Succeed())

		By("waiting for both providers' offers in the catalog")
		Eventually(func(g Gomega) {
			freeBefore = offerCPUs(ctx, central)
			g.Expect(freeBefore).To(HaveLen(2), "offers: %v", freeBefore)
		}, 3*time.Minute, 5*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		By("switching the agents back to central mode")
		_ = kubectlRun(ctx, consumer, "delete", "deployment", scenario.WorkloadName,
			"--namespace", "default", "--ignore-not-found")
		_ = patchAgentConfig(ctx, consumer, map[string]string{"brainMode": "central", "catalogAddr": ""})
		_ = bootstrap.RolloutRestart(ctx, consumer, bootstrap.BrokerNamespace, "agent")
		for _, p := range providers {
			_ = patchAgentConfig(ctx, p, map[string]string{"catalogAddr": ""})
			_ = bootstrap.RolloutRestart(ctx, p, bootstrap.BrokerNamespace, "agent")
		}
	})

	It("scales up with a Reservation the consumer chose from the catalog, and the provider's offer shrinks", func() {
		ctx, cancel := context.WithTimeout(context.Background(), scaleUpTimeout)
		defer cancel()

		var err error
		result, err = scenario.RunHappyPath(ctx, scenario.HappyPathOptions{
			KubeconfigCentral:  central,
			KubeconfigConsumer: consumer,
			WorkloadNamespace:  "default",
			Replicas:           1,
			WaitOptions:        scenario.WaitOptions{Timeout: scaleUpTimeout / 3, Interval: 5 * time.Second},
		})
		Expect(err).NotTo(HaveOccurred())

		provider := reservationProvider(ctx, central, result.ReservationName)
		Expect(freeBefore).To(HaveKey(provider), "the chosen provider %q must have an offer", provider)

		By("the agent logged a local decision")
		logs, err := kubectlOut(ctx, consumer, "logs", "deployment/agent", "--namespace", bootstrap.BrokerNamespace)
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("placement decided locally"))

		By("the provider rewrote its offer with less free capacity")
		Eventually(func() string { return offerCPUs(ctx, central)[provider] }, time.Minute, 2*time.Second).
			ShouldNot(Equal(freeBefore[provider]))
	})

	It("gives the capacity back, and the offer grows again", func() {
		Expect(result).NotTo(BeNil(), "the scale-up spec must have run")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		provider := reservationProvider(ctx, central, result.ReservationName)

		By("deleting the workload and the Reservation")
		Expect(kubectlRun(ctx, consumer, "delete", "deployment", scenario.WorkloadName,
			"--namespace", "default", "--ignore-not-found")).To(Succeed())
		Expect(kubectlDelete(ctx, central, bootstrap.BrokerNamespace,
			"reservations.broker.federation-autoscaler.io/"+result.ReservationName)).To(Succeed())

		Eventually(func() string { return offerCPUs(ctx, central)[provider] }, 3*time.Minute, 2*time.Second).
			Should(Equal(freeBefore[provider]))
	})
})

// kubectlRun runs a kubectl command against kubeconfig and fails with its output.
func kubectlRun(ctx context.Context, kubeconfig string, args ...string) error {
	_, err := kubectlOut(ctx, kubeconfig, args...)
	return err
}

// kubectlOut runs a kubectl command against kubeconfig and returns its output.
func kubectlOut(ctx context.Context, kubeconfig string, args ...string) (string, error) {
	full := append([]string{"--kubeconfig=" + kubeconfig}, args...)
	out, err := exec.CommandContext(ctx, "kubectl", full...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// patchAgentConfig merges data into the agent-config ConfigMap.
func patchAgentConfig(ctx context.Context, kubeconfig string, data map[string]string) error {
	patch, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return err
	}
	return kubectlRun(ctx, kubeconfig, "patch", "configmap", "agent-config",
		"--namespace", bootstrap.BrokerNamespace, "--type", "merge", "--patch", string(patch))
}

// offerCPUs reads every offer in the catalog: provider ID -> free CPU.
func offerCPUs(ctx context.Context, central string) map[string]string {
	out := map[string]string{}
	keys, err := kubectlOut(ctx, central, "exec", "deployment/catalog-redis", "--namespace", bootstrap.BrokerNamespace,
		"--", "redis-cli", "--scan", "--pattern", "catalog:offer:*")
	if err != nil {
		return out
	}
	for _, key := range strings.Fields(keys) {
		raw, err := kubectlOut(ctx, central, "exec", "deployment/catalog-redis", "--namespace", bootstrap.BrokerNamespace,
			"--", "redis-cli", "GET", key)
		if err != nil {
			continue
		}
		var offer struct {
			ProviderID string            `json:"providerId"`
			Available  map[string]string `json:"available"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(raw)), &offer) == nil && offer.ProviderID != "" {
			out[offer.ProviderID] = offer.Available["cpu"]
		}
	}
	return out
}

// reservationProvider returns the provider a Reservation was placed on.
func reservationProvider(ctx context.Context, central, name string) string {
	out, err := kubectlOut(ctx, central, "get", "reservations.broker.federation-autoscaler.io", name,
		"--namespace", bootstrap.BrokerNamespace, "-o", "jsonpath={.spec.providerClusterId}")
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}
