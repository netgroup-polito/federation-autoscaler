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

package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
)

// AgentConfigOptions configures PatchAgentConfig.
type AgentConfigOptions struct {
	// Kubeconfig is the path to the kubeconfig of the target cluster.
	// Required.
	Kubeconfig string

	// Namespace where the agent-config ConfigMap lives. Empty resolves
	// to BrokerNamespace (the federation-autoscaler standard).
	Namespace string

	// Identity carries the values to stamp into the ConfigMap data.
	// Every field is required.
	Identity Identity

	// KubectlBinary overrides the kubectl executable. Empty resolves to
	// $KUBECTL or "kubectl" on $PATH.
	KubectlBinary string

	// CertificateName is the cert-manager Certificate whose CN must be
	// kept in sync with Identity.ClusterID. Defaults to "agent-client"
	// (the name shipped by config/agent/base/certificate.yaml). Empty
	// skips the Certificate patch — useful on clusters that don't run
	// an agent (e.g. the central cluster).
	CertificateName string
}

// PatchAgentConfig kubectl-patches the agent-config ConfigMap on the
// target cluster so the agent reads the right cluster identity at
// startup. Optionally also patches the agent-client Certificate's
// commonName so the broker's mTLS handshake sees a cert whose CN
// matches Identity.ClusterID — see config/agent/base/kustomization.yaml's
// header for why those two values MUST agree.
//
// Idempotent: applying the same patch twice is a no-op.
func PatchAgentConfig(ctx context.Context, opts AgentConfigOptions) error {
	switch {
	case opts.Kubeconfig == "":
		return fmt.Errorf("PatchAgentConfig: Kubeconfig %w", errEmpty)
	case opts.Identity.ClusterID == "":
		return fmt.Errorf("PatchAgentConfig: Identity.ClusterID %w", errEmpty)
	case opts.Identity.LiqoClusterID == "":
		return fmt.Errorf("PatchAgentConfig: Identity.LiqoClusterID %w", errEmpty)
	case opts.Identity.BrokerURL == "":
		return fmt.Errorf("PatchAgentConfig: Identity.BrokerURL %w", errEmpty)
	}
	kubectl, err := resolveBinary(opts.KubectlBinary, envKubectl, defaultKubectlBin)
	if err != nil {
		return err
	}
	namespace := opts.Namespace
	if namespace == "" {
		namespace = BrokerNamespace
	}

	cmPatch, err := json.Marshal(map[string]any{
		"data": map[string]string{
			"clusterId":     opts.Identity.ClusterID,
			"liqoClusterId": opts.Identity.LiqoClusterID,
			"brokerUrl":     opts.Identity.BrokerURL,
		},
	})
	if err != nil {
		return fmt.Errorf("encode configmap patch: %w", err)
	}
	if err := runCommand(ctx, kubectl,
		withKubeconfig(opts.Kubeconfig,
			"patch", "configmap", "agent-config",
			"--namespace", namespace,
			"--type", "merge",
			"--patch", string(cmPatch),
		)...); err != nil {
		return fmt.Errorf("patch agent-config: %w", err)
	}

	if opts.CertificateName == "" {
		return nil
	}
	certPatch, err := json.Marshal(map[string]any{
		"spec": map[string]string{
			"commonName": opts.Identity.ClusterID,
		},
	})
	if err != nil {
		return fmt.Errorf("encode certificate patch: %w", err)
	}
	if err := runCommand(ctx, kubectl,
		withKubeconfig(opts.Kubeconfig,
			"patch", "certificate", opts.CertificateName,
			"--namespace", namespace,
			"--type", "merge",
			"--patch", string(certPatch),
		)...); err != nil {
		return fmt.Errorf("patch certificate %q: %w", opts.CertificateName, err)
	}
	return nil
}
