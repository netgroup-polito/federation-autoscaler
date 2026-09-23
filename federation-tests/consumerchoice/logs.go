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

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/netgroup-polito/federation-autoscaler/federation-tests/testlib"
)

const agentNamespace = "federation-autoscaler-system"

// collectLogs saves component logs before the clusters are torn down, so a run
// can be audited after the fact. Best effort: a missing log is noted in the
// file that would have held it and never fails the run.
func collectLogs(orch *testlib.Orchestrator, rt *ollamaRuntime, outDir string) {
	dir := filepath.Join(outDir, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[logs] create %s: %v", dir, err)
		return
	}
	type source struct{ file, kubeconfig, deployment string }
	sources := []source{{"broker.log", orch.Specs[0].Kubeconfig, "broker"}}
	if orch.Config.Consumers > 0 {
		sources = append(sources,
			source{"consumer-1-agent.log", orch.Specs[1].Kubeconfig, "agent"},
			source{"consumer-1-grpc-server.log", orch.Specs[1].Kubeconfig, "grpc-server"})
	}
	for i := 0; i < orch.Config.Providers; i++ {
		spec := orch.Specs[1+orch.Config.Consumers+i]
		sources = append(sources, source{providerID(i) + "-agent.log", spec.Kubeconfig, "agent"})
	}

	for _, s := range sources {
		if s.kubeconfig == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", s.kubeconfig, "-n", agentNamespace,
			"logs", "deploy/"+s.deployment, "--all-containers", "--timestamps").CombinedOutput()
		cancel()
		if err != nil {
			out = append(out, []byte(fmt.Sprintf("\n[collect] kubectl logs deploy/%s failed: %v\n", s.deployment, err))...)
		}
		if werr := os.WriteFile(filepath.Join(dir, s.file), out, 0o644); werr != nil {
			log.Printf("[logs] write %s: %v", s.file, werr)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if out := rt.logs(ctx); out != nil {
		if err := os.WriteFile(filepath.Join(dir, "ollama.log"), out, 0o644); err != nil {
			log.Printf("[logs] write ollama.log: %v", err)
		}
	}
	log.Printf("[logs] component logs saved to %s", dir)
}
