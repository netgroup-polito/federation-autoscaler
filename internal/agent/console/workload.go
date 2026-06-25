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

package console

import (
	_ "embed"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// workloadYAML is the burst workload the consumer console applies/deletes. It
// is a copy of deploy/ansible/samples/burst-workload.yaml (go:embed cannot
// reach a path outside this package); server_test.go guards against drift.
//
//go:embed assets/burst-workload.yaml
var workloadYAML []byte

// decodeWorkload parses the embedded manifest into a Deployment. A fresh object
// is returned on every call so Create/Delete never share mutated state.
func decodeWorkload() (*appsv1.Deployment, error) {
	var dep appsv1.Deployment
	if err := yaml.Unmarshal(workloadYAML, &dep); err != nil {
		return nil, err
	}
	return &dep, nil
}

// workloadInfo is the display-only shape the consumer UI shows next to the
// apply/delete switch. Present is filled separately by a live Get; the rest
// comes straight from the embedded manifest.
type workloadInfo struct {
	Present  bool              `json:"present"`
	Replicas int32             `json:"replicas"`
	Requests map[string]string `json:"requests"`
	Limits   map[string]string `json:"limits"`
}

// staticWorkloadInfo projects the embedded manifest's sizing (replicas,
// first-container requests/limits). Present defaults to false; the caller sets
// it from a live cluster read.
func staticWorkloadInfo() (workloadInfo, error) {
	dep, err := decodeWorkload()
	if err != nil {
		return workloadInfo{}, err
	}
	info := workloadInfo{
		Requests: map[string]string{},
		Limits:   map[string]string{},
	}
	if dep.Spec.Replicas != nil {
		info.Replicas = *dep.Spec.Replicas
	}
	if len(dep.Spec.Template.Spec.Containers) > 0 {
		res := dep.Spec.Template.Spec.Containers[0].Resources
		info.Requests = resourceListToMap(res.Requests)
		info.Limits = resourceListToMap(res.Limits)
	}
	return info, nil
}

func resourceListToMap(rl corev1.ResourceList) map[string]string {
	out := make(map[string]string, len(rl))
	for name, q := range rl {
		out[string(name)] = q.String()
	}
	return out
}
