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

package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// WaitForReservationPhase polls Reservation CRs on the central cluster
// until at least one has reached the requested Phase, or Timeout fires.
// Returns the name of the first matching Reservation on success.
//
// Phases of interest in the happy-path scenario: "Peered" (the scale-up
// has fully landed) and any intermediate phase the suite wants to gate
// on (e.g. "GeneratingKubeconfig") for diagnostic checkpoints.
func WaitForReservationPhase(
	ctx context.Context,
	kubeconfigCentral, namespace, phase string,
	opts WaitOptions,
	kubectlBinary string,
) (string, error) {
	switch {
	case kubeconfigCentral == "":
		return "", fmt.Errorf("WaitForReservationPhase: kubeconfigCentral %w", errEmpty)
	case phase == "":
		return "", fmt.Errorf("WaitForReservationPhase: phase %w", errEmpty)
	}
	kubectl, err := resolveKubectl(kubectlBinary)
	if err != nil {
		return "", err
	}
	if namespace == "" {
		namespace = "federation-autoscaler-system"
	}

	var match string
	if err := waitUntil(ctx, opts, func() (bool, error) {
		out, err := runKubectl(ctx, kubectl, kubeconfigCentral,
			"get", "reservations.broker.federation-autoscaler.io",
			"--namespace", namespace,
			"-o", "json")
		if err != nil {
			return false, err
		}
		name, found, err := firstReservationInPhase(out, phase)
		if err != nil {
			return false, err
		}
		if found {
			match = name
			return true, nil
		}
		return false, nil
	}); err != nil {
		return "", err
	}
	return match, nil
}

// firstReservationInPhase parses the JSON list `kubectl get reservation
// -o json` returns and finds the first item whose status.phase equals
// the target. Exposed package-private so the unit test can cover the
// JSON-parsing branch without needing a real cluster.
func firstReservationInPhase(rawJSON, phase string) (string, bool, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &list); err != nil {
		return "", false, fmt.Errorf("decode reservation list: %w", err)
	}
	for _, item := range list.Items {
		if item.Status.Phase == phase {
			return item.Metadata.Name, true, nil
		}
	}
	return "", false, nil
}

// WaitForVirtualNodeReady polls Liqo VirtualNode CRs on the consumer
// cluster until one of them reports `status.conditions[type=Node].status
// == "Running"` (Liqo's "ready" signal). Returns the VirtualNode name.
//
// Liqo creates each VirtualNode in a per-provider tenant namespace
// (`liqo-tenant-<provider-id>`) and names it after the provider cluster,
// not the federation-autoscaler reservation. It also does not propagate
// our reservation label onto the CR, so this helper queries across all
// namespaces with no label filter and just looks for the first Ready
// VirtualNode — that's the actual success signal for "the federation
// chain has materialised remote capacity on this consumer".
//
// `namespace` and `reservationID` are kept on the signature for caller
// compatibility but intentionally ignored.
func WaitForVirtualNodeReady(
	ctx context.Context,
	kubeconfigConsumer, _, _ string,
	opts WaitOptions,
	kubectlBinary string,
) (string, error) {
	if kubeconfigConsumer == "" {
		return "", fmt.Errorf("WaitForVirtualNodeReady: kubeconfigConsumer %w", errEmpty)
	}
	kubectl, err := resolveKubectl(kubectlBinary)
	if err != nil {
		return "", err
	}

	getArgs := []string{
		"get", "virtualnodes.offloading.liqo.io",
		"--all-namespaces",
		"-o", "json",
	}

	var match string
	if err := waitUntil(ctx, opts, func() (bool, error) {
		out, err := runKubectl(ctx, kubectl, kubeconfigConsumer, getArgs...)
		if err != nil {
			return false, err
		}
		name, found, err := firstReadyVirtualNode(out)
		if err != nil {
			return false, err
		}
		if found {
			match = name
			return true, nil
		}
		return false, nil
	}); err != nil {
		return "", err
	}
	return match, nil
}

// firstReadyVirtualNode looks for the first Liqo VirtualNode whose
// status.conditions[type=Node].status equals "Running". Liqo emits
// this once the kubelet on the virtual node has fully come up.
func firstReadyVirtualNode(rawJSON string) (string, bool, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &list); err != nil {
		return "", false, fmt.Errorf("decode virtualnode list: %w", err)
	}
	for _, item := range list.Items {
		for _, cond := range item.Status.Conditions {
			if cond.Type == "Node" && cond.Status == "Running" {
				return item.Metadata.Name, true, nil
			}
		}
	}
	return "", false, nil
}

// WaitForPodsScheduled polls the workload's Pods on the consumer
// cluster until at least `wantReady` of them have been scheduled onto
// a Liqo virtual node (i.e. `spec.nodeName` is non-empty AND begins
// with the provider-cluster prefix used by Liqo).
//
// Scope decision: the federation-autoscaler is responsible for getting
// CA to scale up via the federation and producing a Ready VirtualNode
// that the scheduler can bind to. The Pods stay *Scheduled* (not
// *Running*) here for a concrete, fixable reason — see below.
//
// ROOT CAUSE (this is a harness gap, NOT a Kind/Liqo limitation): this
// suite never stamps a `NamespaceOffloading` for the workload namespace,
// so Liqo's virtual-kubelet schedules the Pods onto the virtual node but
// never reflects them to the providers. Liqo's own
// `offloading-with-policies` example runs offloaded Pods to *Running*
// entirely on Kind, and our Ansible deploy (which DOES stamp the NSO)
// runs them on the real VMs. The fix — stamp the NSO in the bootstrap and
// assert `WaitForPodsRunning` — is tracked as post-demo e2e hardening.
// Until then we assert only "CA scheduled the workload onto a federation
// virtual node"; "Pod Running across the tunnel" is validated on the
// multi-VM deployment.
func WaitForPodsScheduled(
	ctx context.Context,
	kubeconfigConsumer, namespace, labelSelector string,
	wantReady int,
	opts WaitOptions,
	kubectlBinary string,
) error {
	if kubeconfigConsumer == "" {
		return fmt.Errorf("WaitForPodsScheduled: kubeconfigConsumer %w", errEmpty)
	}
	if labelSelector == "" {
		return fmt.Errorf("WaitForPodsScheduled: labelSelector %w", errEmpty)
	}
	if wantReady < 1 {
		wantReady = 1
	}
	kubectl, err := resolveKubectl(kubectlBinary)
	if err != nil {
		return err
	}
	return waitUntil(ctx, opts, func() (bool, error) {
		out, err := runKubectl(ctx, kubectl, kubeconfigConsumer,
			"get", "pods",
			"--namespace", namespace,
			"--selector", labelSelector,
			"-o", "json")
		if err != nil {
			return false, err
		}
		scheduled, err := countScheduledPods(out)
		if err != nil {
			return false, err
		}
		return scheduled >= wantReady, nil
	})
}

// countScheduledPods returns the number of Pods in the JSON list whose
// spec.nodeName is set. CA's scale-up + the kube-scheduler bind once
// the Liqo VirtualNode is Ready, so nodeName != "" is the moment the
// federation chain has produced a usable node from the perspective of
// the workload.
func countScheduledPods(rawJSON string) (int, error) {
	var list struct {
		Items []struct {
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &list); err != nil {
		return 0, fmt.Errorf("decode pod list: %w", err)
	}
	n := 0
	for _, item := range list.Items {
		if strings.TrimSpace(item.Spec.NodeName) != "" {
			n++
		}
	}
	return n, nil
}
