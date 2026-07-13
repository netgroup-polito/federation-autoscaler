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

// Command mock-geo is a standalone HTTP stand-in for an ip-api.com-style geo-IP
// API, used by automatic location discovery. It serves GET /json/<ip> with that
// IP's location, resolved by longest-prefix CIDR match. Deployed on the dedicated
// mock cluster; both agent roles geolocate their own node IP via --mock-geo-url.
package main

import (
	"flag"
	"log"

	"github.com/netgroup-polito/federation-autoscaler/internal/mockgeo"
)

func main() {
	port := flag.Int("port", 8080, "TCP port to listen on")
	flag.Parse()

	if err := mockgeo.StartServer(*port); err != nil {
		log.Fatalf("mock-geo server failed: %v", err)
	}
}
