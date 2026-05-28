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

package integration

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/health"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/poller"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

var _ = Describe("Step 7 end-to-end: agent client + poller against the real broker over mTLS", func() {
	const (
		providerCluster = "provider-7e"
		consumerCluster = "consumer-7e"
		resName         = "agent-e2e-resv"
	)

	resvKey := types.NamespacedName{Name: resName, Namespace: suiteNamespace}
	// GenerateKubeconfig is (consumer, provider)-keyed (bug #5), not per-reservation.
	gkKey := types.NamespacedName{Name: "gk-" + consumerCluster + "-" + providerCluster, Namespace: suiteNamespace}
	cadvKey := types.NamespacedName{Name: providerCluster, Namespace: suiteNamespace}

	AfterEach(func() {
		// Best-effort cleanup. envtest does not run the GC.
		_ = k8sClient.Delete(suiteCtx, &autoscalingv1alpha1.ProviderInstruction{
			ObjectMeta: metav1.ObjectMeta{Name: gkKey.Name, Namespace: suiteNamespace},
		})
		_ = k8sClient.Delete(suiteCtx, &brokerv1alpha1.Reservation{
			ObjectMeta: metav1.ObjectMeta{Name: resName, Namespace: suiteNamespace},
		})
		_ = k8sClient.Delete(suiteCtx, &brokerv1alpha1.ClusterAdvertisement{
			ObjectMeta: metav1.ObjectMeta{Name: providerCluster, Namespace: suiteNamespace},
		})
	})

	It("dispatches GenerateKubeconfig through the provider agent's poller and the broker advances the Reservation phase", func() {
		By("issuing a CN=provider-7e client cert signed by the test CA")
		agentCert, err := bundleBuilder_.issueAgentCert(providerCluster, 100)
		Expect(err).NotTo(HaveOccurred())

		By("creating an available ClusterAdvertisement so the reservation guard passes")
		cadv := &brokerv1alpha1.ClusterAdvertisement{
			ObjectMeta: metav1.ObjectMeta{Name: providerCluster, Namespace: suiteNamespace},
			Spec: brokerv1alpha1.ClusterAdvertisementSpec{
				ClusterID:     providerCluster,
				LiqoClusterID: "liqo-" + providerCluster,
				ClusterType:   brokerv1alpha1.ChunkTypeStandard,
				Resources: brokerv1alpha1.AdvertisedResources{
					Allocatable: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("8"),
						corev1.ResourceMemory: resource.MustParse("16Gi"),
					},
				},
			},
		}
		Expect(k8sClient.Create(suiteCtx, cadv)).To(Succeed())
		Eventually(func() error {
			c := &brokerv1alpha1.ClusterAdvertisement{}
			if err := k8sClient.Get(suiteCtx, cadvKey, c); err != nil {
				return err
			}
			now := metav1.Now()
			c.Status.Available = true
			c.Status.LastSeen = &now
			return k8sClient.Status().Update(suiteCtx, c)
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("creating a Reservation in Pending so the reconciler emits GenerateKubeconfig")
		resv := &brokerv1alpha1.Reservation{
			ObjectMeta: metav1.ObjectMeta{Name: resName, Namespace: suiteNamespace},
			Spec: brokerv1alpha1.ReservationSpec{
				ConsumerClusterID:     consumerCluster,
				ConsumerLiqoClusterID: "liqo-" + consumerCluster,
				ProviderClusterID:     providerCluster,
				ProviderLiqoClusterID: "liqo-" + providerCluster,
				ChunkCount:            1,
				ChunkType:             brokerv1alpha1.ChunkTypeStandard,
				Resources: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
			},
		}
		Expect(k8sClient.Create(suiteCtx, resv)).To(Succeed())
		Eventually(func() error {
			r := &brokerv1alpha1.Reservation{}
			if err := k8sClient.Get(suiteCtx, resvKey, r); err != nil {
				return err
			}
			r.Status.Phase = brokerv1alpha1.ReservationPhasePending
			return k8sClient.Status().Update(suiteCtx, r)
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("waiting for the reconciler to emit the GenerateKubeconfig instruction")
		Eventually(func() error {
			pi := &autoscalingv1alpha1.ProviderInstruction{}
			return k8sClient.Get(suiteCtx, gkKey, pi)
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("building the agent's broker client over real mTLS")
		brokerClient, err := agentclient.New(agentclient.Options{
			BrokerURL: brokerURL(),
			TLS: agentclient.TLSConfig{
				CertFile:     agentCert.certPath,
				KeyFile:      agentCert.keyPath,
				BrokerCAFile: bundle.caPath,
				// httptest.Server SAN included 127.0.0.1; pin ServerName
				// so the handshake verifies the cert against an explicit
				// host even though we dial by IP.
				ServerName: serverNameFromAddr(brokerListen),
			},
			RequestTimeout: 5 * time.Second,
			MaxRetries:     2,
			InitialBackoff: 50 * time.Millisecond,
			MaxBackoff:     200 * time.Millisecond,
		})
		Expect(err).NotTo(HaveOccurred())

		By("registering a stub provider GenerateKubeconfig handler that returns a fake kubeconfig")
		var seenInstruction atomic.Pointer[brokerapi.InstructionView]
		registry := poller.NewRegistry()
		registry.RegisterProvider(autoscalingv1alpha1.ProviderInstructionGenerateKubeconfig,
			func(_ context.Context, in *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
				cp := *in
				seenInstruction.Store(&cp)
				return &brokerapi.InstructionResultRequest{
					Status: brokerapi.ResultStatusSucceeded,
					Payload: &brokerapi.ResultPayload{
						Kind:       brokerapi.PayloadKindKubeconfig,
						Kubeconfig: "ZmFrZS1rdWJlY29uZmln", // base64("fake-kubeconfig")
					},
				}, nil
			})

		probe := health.New(health.Options{PollStaleAfter: 5 * time.Second})

		p, err := poller.New(poller.Options{
			Client:       brokerClient,
			Registry:     registry,
			Interval:     200 * time.Millisecond,
			OnPollResult: probe.RecordPoll,
		})
		Expect(err).NotTo(HaveOccurred())

		pollerCtx, pollerCancel := context.WithCancel(suiteCtx)
		pollerDone := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			p.Run(pollerCtx)
			close(pollerDone)
		}()
		defer func() {
			pollerCancel()
			Eventually(pollerDone, time.Second, 50*time.Millisecond).Should(BeClosed())
		}()

		By("the poller picked up the instruction and the handler observed it")
		Eventually(func() *brokerapi.InstructionView {
			return seenInstruction.Load()
		}, suiteTimeout, suiteInterval).ShouldNot(BeNil())
		got := seenInstruction.Load()
		Expect(got.Kind).To(Equal(string(autoscalingv1alpha1.ProviderInstructionGenerateKubeconfig)))
		Expect(got.ReservationID).To(Equal(resName))

		By("the broker recorded the result — instruction Status.Enforced flips to true")
		Eventually(func(g Gomega) {
			pi := &autoscalingv1alpha1.ProviderInstruction{}
			g.Expect(k8sClient.Get(suiteCtx, gkKey, pi)).To(Succeed())
			g.Expect(pi.Status.Enforced).To(BeTrue())
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("the broker advanced the Reservation past GeneratingKubeconfig")
		// applyKubeconfigPayload moves Reservation → KubeconfigReady. The
		// reservation reconciler immediately picks it up and may already
		// have advanced to Peering (and emitted the Peer instruction) by
		// the time we sample, so we accept any post-kubeconfig phase as
		// proof the broker processed the agent's result.
		Eventually(func(g Gomega) {
			r := &brokerv1alpha1.Reservation{}
			g.Expect(k8sClient.Get(suiteCtx, resvKey, r)).To(Succeed())
			g.Expect(r.Status.Phase).To(BeElementOf(
				brokerv1alpha1.ReservationPhaseKubeconfigReady,
				brokerv1alpha1.ReservationPhasePeering,
				brokerv1alpha1.ReservationPhasePeered,
			))
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("the readiness probe is green — the poll loop is healthy")
		Expect(probe.Ready()).To(Succeed())
	})
})

// serverNameFromAddr extracts the host portion of a host:port string so
// the agent's TLS handshake verifies the broker's cert SAN against the
// dialled host, not the host:port literal. The test broker's cert
// covers 127.0.0.1 / ::1 / localhost (see issueServer).
func serverNameFromAddr(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
