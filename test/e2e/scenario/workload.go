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
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"text/template"
)

// WorkloadName is the canonical Deployment name the suite uses for its
// synthetic unschedulable workload. Pinned so the assertions can target
// it by label / name across waitFor steps.
const WorkloadName = "federation-scaleup-driver"

// WorkloadOptions configures CreateUnschedulableWorkload.
type WorkloadOptions struct {
	// Kubeconfig is the consumer cluster's kubeconfig. Required.
	Kubeconfig string

	// Namespace where the Deployment lands. Required.
	Namespace string

	// Replicas is how many Pods to schedule. Each Pod alone is small
	// enough for a federation chunk to satisfy but the aggregate is
	// large enough to force CA to scale up — the suite picks
	// Replicas such that local nodes can't hold them all.
	Replicas int

	// CPURequest / MemoryRequest are the per-Pod requests. Defaults
	// to 1500m / 1Gi which is too big for a default Kind worker but
	// fits inside the standard chunk (2 cpu / 4Gi from the
	// chunk-config sample).
	CPURequest    string
	MemoryRequest string

	// Image is the placeholder container image. Defaults to a
	// `pause` image so the Pods sleep forever once scheduled — the
	// suite only cares about the Scheduled condition, not workload
	// behaviour.
	Image string

	// KubectlBinary overrides the kubectl executable.
	KubectlBinary string
}

const workloadTemplate = `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
  labels:
    app.kubernetes.io/name: {{ .Name }}
spec:
  replicas: {{ .Replicas }}
  selector:
    matchLabels:
      app.kubernetes.io/name: {{ .Name }}
  template:
    metadata:
      labels:
        app.kubernetes.io/name: {{ .Name }}
    spec:
      # No toleration for the Liqo virtual-node taint — Kubernetes will
      # only schedule onto a Liqo virtual-node when CA explicitly
      # provisions it AND adds the matching toleration; in our model
      # CA's NodeGroupTemplate already strips the taint, so untolerated
      # pods are admitted.
      containers:
      - name: pause
        image: {{ .Image }}
        resources:
          requests:
            cpu: {{ .CPURequest }}
            memory: {{ .MemoryRequest }}
        # Force the container to sleep forever; the suite never reads
        # its logs.
        command: ["/pause"]
`

type workloadData struct {
	Name          string
	Namespace     string
	Replicas      int
	CPURequest    string
	MemoryRequest string
	Image         string
}

// renderWorkloadManifest expands the workload template and returns the
// rendered YAML. Exposed package-private so the unit test can assert
// against it without needing kubectl.
func renderWorkloadManifest(opts WorkloadOptions) (string, error) {
	d := workloadData{
		Name:          WorkloadName,
		Namespace:     opts.Namespace,
		Replicas:      opts.Replicas,
		CPURequest:    opts.CPURequest,
		MemoryRequest: opts.MemoryRequest,
		Image:         opts.Image,
	}
	if d.Replicas <= 0 {
		d.Replicas = 1
	}
	if d.CPURequest == "" {
		d.CPURequest = "1500m"
	}
	if d.MemoryRequest == "" {
		d.MemoryRequest = "1Gi"
	}
	if d.Image == "" {
		d.Image = "registry.k8s.io/pause:3.10"
	}
	tmpl, err := template.New("workload").Parse(workloadTemplate)
	if err != nil {
		return "", fmt.Errorf("parse workload template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, d); err != nil {
		return "", fmt.Errorf("render workload manifest: %w", err)
	}
	return buf.String(), nil
}

// CreateUnschedulableWorkload renders a synthetic Deployment whose Pods
// can't fit on the consumer cluster's local nodes (per-Pod requests >
// any single local node's allocatable) and applies it via kubectl.
// Idempotent — `kubectl apply` updates an existing Deployment in place.
//
// The Pods stay Pending until CA scales up by carving a Reservation
// out of the federation, which is the assertion target.
func CreateUnschedulableWorkload(ctx context.Context, opts WorkloadOptions) error {
	switch {
	case opts.Kubeconfig == "":
		return fmt.Errorf("CreateUnschedulableWorkload: Kubeconfig %w", errEmpty)
	case opts.Namespace == "":
		return fmt.Errorf("CreateUnschedulableWorkload: Namespace %w", errEmpty)
	}
	kubectl, err := resolveKubectl(opts.KubectlBinary)
	if err != nil {
		return err
	}
	manifest, err := renderWorkloadManifest(opts)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, kubectl,
		"--kubeconfig="+opts.Kubeconfig, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl apply workload: %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}
