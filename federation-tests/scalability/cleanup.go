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

// Cleanup removes only the ClusterAdvertisement CRs this harness created.
//
// CORRECTION vs. the original design intent: internal/broker/api/advertisement.go's
// upsertClusterAdvertisement only ever writes cadv.Spec — it never sets
// ObjectMeta.Labels — so there is no server-side label a client can attach
// to a ClusterAdvertisement to mark it as test-created; "the Broker must
// label test resources" was not something the actually-implemented POST
// /api/v1/advertisements handler supports, and this harness does not
// invent a request field the Broker ignores. The one real, unambiguous
// signal is the CR's NAME, which is always exactly the advertising
// cluster's ClusterID (name: req.ClusterID in upsertClusterAdvertisement) —
// and every logical provider in this harness authenticates with a cert
// whose CN, and therefore whose ClusterID, is generate-test-certs.sh's
// "scaltest-provider-NNN" convention (certs.go). Cleanup matches on that
// name prefix and nothing else, so it can never touch a real provider's
// ClusterAdvertisement (which would need to name itself "scaltest-..." to
// collide — vanishingly unlikely and easy to avoid).
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os/exec"
	"strings"
)

const clusterAdvertisementResource = "clusteradvertisements.broker.federation-autoscaler.io"

func runCleanup(args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ExitOnError)
	namespace := fs.String("namespace", "federation-autoscaler-system", "Namespace holding the ClusterAdvertisement CRs (matches config/broker/deployment.yaml).")
	kubeconfig := fs.String("kubeconfig", "", "Kubeconfig path passed to kubectl; empty uses kubectl's default resolution.")
	prefix := fs.String("prefix", clusterIDPrefix+"-", "Only ClusterAdvertisement CR names starting with this prefix are ever touched.")
	yes := fs.Bool("yes", false, "Actually delete. Without this flag, cleanup only lists what it would delete (dry run).")
	if err := fs.Parse(args); err != nil {
		return err
	}

	kubectlArgs := func(rest ...string) []string {
		a := []string{}
		if *kubeconfig != "" {
			a = append(a, "--kubeconfig", *kubeconfig)
		}
		return append(a, rest...)
	}

	out, err := exec.Command("kubectl", kubectlArgs("get", clusterAdvertisementResource,
		"-n", *namespace, "-o", "name")...).Output()
	if err != nil {
		return fmt.Errorf("kubectl get %s: %w", clusterAdvertisementResource, err)
	}

	var names []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		// line looks like "clusteradvertisement.broker.federation-autoscaler.io/scaltest-provider-001"
		idx := strings.LastIndex(line, "/")
		if idx < 0 {
			continue
		}
		name := line[idx+1:]
		if strings.HasPrefix(name, *prefix) {
			names = append(names, name)
		}
	}

	if len(names) == 0 {
		fmt.Println("cleanup: no ClusterAdvertisement CRs matching prefix", *prefix, "in namespace", *namespace)
		return nil
	}

	fmt.Printf("cleanup: %d ClusterAdvertisement CR(s) matching prefix %q in namespace %s:\n", len(names), *prefix, *namespace)
	for _, n := range names {
		fmt.Println("  -", n)
	}

	if !*yes {
		fmt.Println("\ndry run — pass --yes to actually delete these (and only these) resources")
		return nil
	}

	deleteArgs := append([]string{"delete", clusterAdvertisementResource, "-n", *namespace}, names...)
	cmd := exec.Command("kubectl", kubectlArgs(deleteArgs...)...)
	cmdOut, err := cmd.CombinedOutput()
	fmt.Print(string(cmdOut))
	if err != nil {
		return fmt.Errorf("kubectl delete: %w", err)
	}
	return nil
}
