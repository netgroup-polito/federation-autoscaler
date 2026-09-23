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

// Command mock-eco-controllable runs the controllable mock-eco carbon service.
// Deploy on the mock cluster as a drop-in replacement for the production
// mock-eco, then use POST /admin/carbon to set per-region carbon values
// from the test harness.
package main

import (
	"flag"
	"log"

	mockecotest "github.com/netgroup-polito/federation-autoscaler/federation-tests/mock-eco-test"
)

func main() {
	port := flag.Int("port", 8080, "HTTP listen port")
	flag.Parse()

	if err := mockecotest.StartServer(*port); err != nil {
		log.Fatalf("mock-eco-controllable: %v", err)
	}
}
