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
	"fmt"
)

// CertManagerVersion is the pinned cert-manager release the e2e suite
// installs on every cluster. Kept in sync with test/utils/utils.go's
// constant so a single bump moves both the kubebuilder scaffold tests
// and the multi-cluster e2e suite.
const CertManagerVersion = "v1.18.2"

// certManagerManifestURLFmt is the release manifest URL pattern.
const certManagerManifestURLFmt = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"

// CertManagerOptions configures InstallCertManager.
type CertManagerOptions struct {
	// Kubeconfig is the path to the kubeconfig of the target cluster.
	// Required.
	Kubeconfig string

	// KubectlBinary overrides the kubectl executable. Empty resolves to
	// $KUBECTL or "kubectl" on $PATH.
	KubectlBinary string

	// Version overrides the cert-manager release. Empty resolves to
	// CertManagerVersion.
	Version string
}

// InstallCertManager applies the pinned cert-manager release manifest
// to the target cluster, then waits for the webhook Deployment to
// become Available. Idempotent: a second invocation no-ops because the
// apply just re-asserts the existing objects.
func InstallCertManager(ctx context.Context, opts CertManagerOptions) error {
	if opts.Kubeconfig == "" {
		return fmt.Errorf("InstallCertManager: Kubeconfig %w", errEmpty)
	}
	kubectl, err := resolveBinary(opts.KubectlBinary, envKubectl, defaultKubectlBin)
	if err != nil {
		return err
	}
	version := opts.Version
	if version == "" {
		version = CertManagerVersion
	}
	url := fmt.Sprintf(certManagerManifestURLFmt, version)

	if err := runCommand(ctx, kubectl,
		withKubeconfig(opts.Kubeconfig, "apply", "-f", url)...); err != nil {
		return fmt.Errorf("apply cert-manager manifest: %w", err)
	}
	if err := runCommand(ctx, kubectl,
		withKubeconfig(opts.Kubeconfig,
			"wait", "deployment.apps/cert-manager-webhook",
			"--for", "condition=Available",
			"--namespace", "cert-manager",
			"--timeout", "5m",
		)...); err != nil {
		return fmt.Errorf("wait for cert-manager-webhook: %w", err)
	}
	return nil
}
