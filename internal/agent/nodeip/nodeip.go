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

// Package nodeip resolves the IP address an agent should be geolocated by. It is
// the single source of the "which IP am I" logic shared by the provider
// publisher, the consumer heartbeat, and the config console: an explicit
// --advertised-ip override wins; otherwise the agent's own node IP is read from
// v1.Node (ExternalIP preferred, InternalIP fallback — the routable private
// address in an on-prem env).
package nodeip

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Resolve returns the IP to geolocate this agent by.
//
//   - override (from --advertised-ip) non-empty ⇒ returned verbatim (the demo
//     steering lever / correctness escape); the node is not read.
//   - otherwise the node named nodeName is read and its address chosen:
//     ExternalIP first, then InternalIP.
//
// An empty nodeName or nil client returns ("", nil) — location discovery is
// simply off. A node that exists but exposes no usable address returns ("", nil)
// too. Only a failed node read returns a non-nil error; callers log it and
// proceed without a location (never fatal).
func Resolve(ctx context.Context, c client.Client, nodeName, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if c == nil || nodeName == "" {
		return "", nil
	}

	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return "", fmt.Errorf("nodeip: get node %q: %w", nodeName, err)
	}

	var internal string
	for _, addr := range node.Status.Addresses {
		switch addr.Type {
		case corev1.NodeExternalIP:
			if addr.Address != "" {
				return addr.Address, nil
			}
		case corev1.NodeInternalIP:
			if internal == "" {
				internal = addr.Address
			}
		}
	}
	return internal, nil
}
