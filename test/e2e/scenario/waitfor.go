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
// The reservation label is set by the consumer agent's Peer handler
// (see config/agent/ + internal/agent/consumer/instructions/liqo.go);
// Liqo propagates it onto the VirtualNode it materialises, so we can
// scope the lookup to the reservation we care about.
func WaitForVirtualNodeReady(
	ctx context.Context,
	kubeconfigConsumer, namespace, reservationID string,
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
	if namespace == "" {
		namespace = "federation-autoscaler-system"
	}

	getArgs := []string{
		"get", "virtualnodes.offloading.liqo.io",
		"--namespace", namespace,
		"-o", "json",
	}
	if reservationID != "" {
		getArgs = append(getArgs,
			"-l", "federation-autoscaler.io/reservation="+reservationID)
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
// cluster until at least `wantReady` of them have status.phase ==
// "Running". The synthetic workload uses the pause image, so reaching
// Running is the strongest signal we can get that the Liqo virtual
// node is genuinely usable end-to-end.
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
		running, err := countRunningPods(out)
		if err != nil {
			return false, err
		}
		return running >= wantReady, nil
	})
}

// countRunningPods returns the number of Pods in the JSON list whose
// status.phase == "Running".
func countRunningPods(rawJSON string) (int, error) {
	var list struct {
		Items []struct {
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &list); err != nil {
		return 0, fmt.Errorf("decode pod list: %w", err)
	}
	n := 0
	for _, item := range list.Items {
		if strings.EqualFold(item.Status.Phase, "Running") {
			n++
		}
	}
	return n, nil
}
