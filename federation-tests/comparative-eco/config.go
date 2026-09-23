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
	"flag"
	"fmt"
	"os"
)

func parseFlags() (configPath string, keepClusters, skipBuild bool, runID string) {
	flag.StringVar(&configPath, "config", "", "Path to the experiment YAML config file.")
	flag.BoolVar(&keepClusters, "keep-clusters", false, "Skip cleanup (keep Kind clusters for debugging).")
	flag.BoolVar(&skipBuild, "skip-build", false, "Skip make docker-build (use pre-built images).")
	flag.StringVar(&runID, "run-id", "", "Override run ID (default: auto-generated).")
	flag.Parse()
	if configPath == "" {
		fmt.Fprintln(os.Stderr, "usage: comparative-eco --config <config.yaml>")
		os.Exit(1)
	}
	return
}
