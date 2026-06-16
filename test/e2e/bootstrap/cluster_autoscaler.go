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
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"text/template"
)

// ClusterAutoscalerImage is the upstream Cluster Autoscaler image the
// e2e suite deploys onto the consumer cluster. Pinned to a 1.32.x
// release that supports the externalgrpc cloud provider.
const ClusterAutoscalerImage = "registry.k8s.io/autoscaling/cluster-autoscaler:v1.32.0"

// caManifestTemplate emits every Kubernetes object Cluster Autoscaler
// needs to run on the consumer cluster with --cloud-provider=externalgrpc:
//
//   - ServiceAccount + ClusterRole + ClusterRoleBinding (broad permissions
//     because CA reaches across the whole cluster's pods / nodes; the
//     e2e suite intentionally uses cluster-admin to keep the template
//     small — production deploys MUST use the upstream scoped role from
//     cluster-autoscaler/charts/cluster-autoscaler.)
//   - Certificate (cert-manager) for CA's externalgrpc client cert,
//     signed by the broker's CA-Issuer so the gRPC server trusts the chain
//   - Secret (in-line) carrying the cloud-config YAML referencing the cert
//     files mounted from the cert-manager-produced Secret
//   - Deployment running ClusterAutoscalerImage with the right args
//
// The expander is `least-waste` — a neutral, NON-price expander. Price-based
// provider selection is the BROKER's job: it masks GET /api/v1/nodegroups per
// consumer that opts in via a ConsumerPolicy{placement.type: Price}, exposing
// only the cheapest priced provider with capacity. CA therefore stays
// price-agnostic (a `price` expander here would price-select for EVERY
// consumer, defeating the per-consumer opt-in). externalgrpc only exposes
// federation node groups, so any expander scales a federation group; the
// broker's masking is what makes the cheapest provider win.
const caManifestTemplate = `---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: cluster-autoscaler
  namespace: {{ .Namespace }}
  labels:
    app.kubernetes.io/name: cluster-autoscaler
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: cluster-autoscaler
  labels:
    app.kubernetes.io/name: cluster-autoscaler
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
- kind: ServiceAccount
  name: cluster-autoscaler
  namespace: {{ .Namespace }}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: cluster-autoscaler-client
  namespace: {{ .Namespace }}
spec:
  commonName: cluster-autoscaler
  secretName: cluster-autoscaler-client-cert
  duration: 8760h
  renewBefore: 720h
  privateKey:
    algorithm: ECDSA
    size: 256
  usages:
  - client auth
  issuerRef:
    name: federation-autoscaler-ca-issuer
    kind: Issuer
    group: cert-manager.io
---
apiVersion: v1
kind: Secret
metadata:
  name: cluster-autoscaler-cloud-config
  namespace: {{ .Namespace }}
type: Opaque
stringData:
  cloud-config.yaml: |
    address: {{ .GRPCServerAddress }}
    key: /etc/cluster-autoscaler/cert/tls.key
    cert: /etc/cluster-autoscaler/cert/tls.crt
    cacert: /etc/cluster-autoscaler/cert/ca.crt
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cluster-autoscaler
  namespace: {{ .Namespace }}
  labels:
    app.kubernetes.io/name: cluster-autoscaler
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: cluster-autoscaler
  template:
    metadata:
      labels:
        app.kubernetes.io/name: cluster-autoscaler
    spec:
      serviceAccountName: cluster-autoscaler
      containers:
      - name: cluster-autoscaler
        image: {{ .Image }}
        imagePullPolicy: IfNotPresent
        command:
        - ./cluster-autoscaler
        - --v=4
        - --cloud-provider=externalgrpc
        - --cloud-config=/etc/cluster-autoscaler/config/cloud-config.yaml
        - --expander=least-waste
        - --scale-down-enabled=false
        - --skip-nodes-with-local-storage=false
        - --skip-nodes-with-system-pods=false
        - --scan-interval=10s
        volumeMounts:
        - name: cert
          mountPath: /etc/cluster-autoscaler/cert
          readOnly: true
        - name: config
          mountPath: /etc/cluster-autoscaler/config
          readOnly: true
      volumes:
      - name: cert
        secret:
          secretName: cluster-autoscaler-client-cert
      - name: config
        secret:
          secretName: cluster-autoscaler-cloud-config
`

// ClusterAutoscalerOptions configures DeployClusterAutoscaler.
type ClusterAutoscalerOptions struct {
	// Kubeconfig is the consumer cluster's kubeconfig. Required.
	Kubeconfig string

	// Namespace where CA lives. Empty defaults to BrokerNamespace —
	// for the e2e suite that's federation-autoscaler-system so
	// CA's client Certificate finds the same federation-autoscaler-ca-
	// issuer the agent / grpc-server certs chain to.
	Namespace string

	// GRPCServerAddress is the host:port CA dials. Defaults to the
	// in-cluster Service of the gRPC server (port 9443).
	GRPCServerAddress string

	// Image overrides ClusterAutoscalerImage. Empty resolves to that
	// default constant.
	Image string

	// KubectlBinary overrides the kubectl executable. Empty resolves
	// to $KUBECTL or "kubectl".
	KubectlBinary string
}

// caTemplateData is the struct text/template renders against.
type caTemplateData struct {
	Namespace         string
	GRPCServerAddress string
	Image             string
}

// renderClusterAutoscalerManifest expands the template into a single
// multi-document YAML string. Exposed (lowercase first letter — package-
// private) so the unit test can verify the rendering without applying.
func renderClusterAutoscalerManifest(opts ClusterAutoscalerOptions) (string, error) {
	data := caTemplateData{
		Namespace:         opts.Namespace,
		GRPCServerAddress: opts.GRPCServerAddress,
		Image:             opts.Image,
	}
	if data.Namespace == "" {
		data.Namespace = BrokerNamespace
	}
	if data.GRPCServerAddress == "" {
		data.GRPCServerAddress = defaultGRPCServerAddress()
	}
	if data.Image == "" {
		data.Image = ClusterAutoscalerImage
	}
	tmpl, err := template.New("ca").Parse(caManifestTemplate)
	if err != nil {
		return "", fmt.Errorf("parse CA manifest template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render CA manifest: %w", err)
	}
	return buf.String(), nil
}

// DeployClusterAutoscaler renders the CA manifest bundle and applies
// it to the consumer cluster via `kubectl apply -f -`. Idempotent.
func DeployClusterAutoscaler(ctx context.Context, opts ClusterAutoscalerOptions) error {
	if opts.Kubeconfig == "" {
		return fmt.Errorf("DeployClusterAutoscaler: Kubeconfig %w", errEmpty)
	}
	kubectl, err := resolveBinary(opts.KubectlBinary, envKubectl, defaultKubectlBin)
	if err != nil {
		return err
	}
	manifest, err := renderClusterAutoscalerManifest(opts)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, kubectl,
		"--kubeconfig="+opts.Kubeconfig, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl apply CA: %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

// defaultGRPCServerAddress is the cluster-local address CA dials. The
// grpc-server Service is named "grpc-server" in BrokerNamespace and
// listens on port 9443 — see config/grpc-server/service.yaml.
func defaultGRPCServerAddress() string {
	return fmt.Sprintf("grpc-server.%s.svc.cluster.local:9443", BrokerNamespace)
}
