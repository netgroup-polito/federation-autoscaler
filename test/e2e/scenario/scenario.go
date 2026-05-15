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
	"fmt"
)

// HappyPathOptions configures RunHappyPath.
type HappyPathOptions struct {
	// KubeconfigCentral / KubeconfigConsumer are the per-cluster
	// kubeconfig paths the kind.Topology produced (step 13a). Required.
	KubeconfigCentral  string
	KubeconfigConsumer string

	// Namespace is where the synthetic workload + assertions live. The
	// federation-autoscaler artefacts (broker, agent, VirtualNodeStates)
	// are deployed into federation-autoscaler-system by the bootstrap
	// helpers; the workload lives in its own namespace by default so
	// it can be torn down separately. Defaults to "default".
	WorkloadNamespace string

	// Replicas is how many synthetic Pods to schedule. Pick a value
	// such that the consumer cluster's local nodes can't hold them all
	// — the suite typically uses 2 with 1500m CPU each, exceeding a
	// default Kind worker's allocatable.
	Replicas int

	// WaitOptions overrides the per-step Timeout/Interval. Zero values
	// fall back to DefaultTimeout / DefaultInterval.
	WaitOptions WaitOptions
}

// HappyPathResult summarises which Reservation and VirtualNode the
// scenario ended up driving through to Peered/Ready. Useful for
// downstream tear-down assertions in step 13e.
type HappyPathResult struct {
	ReservationName string
	VirtualNodeName string
}

// RunHappyPath drives the federation scale-up end-to-end:
//
//  1. Schedule the unschedulable synthetic workload on the consumer.
//  2. Wait for the broker to spawn a Reservation that reaches Peered.
//  3. Wait for the corresponding Liqo VirtualNode to become Ready on
//     the consumer cluster.
//  4. Wait for the workload's Pods to become Running (i.e. scheduled
//     onto the new virtual node).
//
// Each step has its own WaitOptions-bounded poll loop. Returns the
// names of the Reservation / VirtualNode the scenario observed.
func RunHappyPath(ctx context.Context, opts HappyPathOptions) (*HappyPathResult, error) {
	switch {
	case opts.KubeconfigCentral == "":
		return nil, fmt.Errorf("RunHappyPath: KubeconfigCentral %w", errEmpty)
	case opts.KubeconfigConsumer == "":
		return nil, fmt.Errorf("RunHappyPath: KubeconfigConsumer %w", errEmpty)
	}
	ns := opts.WorkloadNamespace
	if ns == "" {
		ns = "default"
	}
	replicas := opts.Replicas
	if replicas <= 0 {
		replicas = 2
	}

	if err := CreateUnschedulableWorkload(ctx, WorkloadOptions{
		Kubeconfig: opts.KubeconfigConsumer,
		Namespace:  ns,
		Replicas:   replicas,
	}); err != nil {
		return nil, fmt.Errorf("create synthetic workload: %w", err)
	}

	reservation, err := WaitForReservationPhase(ctx,
		opts.KubeconfigCentral, "", "Peered", opts.WaitOptions, "")
	if err != nil {
		return nil, fmt.Errorf("wait for Reservation Peered: %w", err)
	}

	vnName, err := WaitForVirtualNodeReady(ctx,
		opts.KubeconfigConsumer, "", reservation, opts.WaitOptions, "")
	if err != nil {
		return nil, fmt.Errorf("wait for VirtualNode Ready: %w", err)
	}

	if err := WaitForPodsScheduled(ctx,
		opts.KubeconfigConsumer, ns,
		"app.kubernetes.io/name="+WorkloadName,
		replicas, opts.WaitOptions, ""); err != nil {
		return nil, fmt.Errorf("wait for Pods Scheduled: %w", err)
	}

	return &HappyPathResult{
		ReservationName: reservation,
		VirtualNodeName: vnName,
	}, nil
}
