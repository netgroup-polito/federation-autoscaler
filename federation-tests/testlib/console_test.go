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

package testlib

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The console client speaks the consumer console's real wire format: the body
// of GET /api/state (internal/agent/console/state.go consumerState) and the
// POST /api/reservation actions the console accepts.
func TestConsoleClient_ManualReservations(t *testing.T) {
	var posted []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/state":
			_, _ = w.Write([]byte(`{"role":"consumer","clusterId":"consumer-1","policy":"ConsumerChoice",
				"userPrompt":"close to me","location":{"region":"LOM","lat":45.46,"lon":9.19},
				"manualReservations":[{"name":"rr-abc","phase":"Active","provider":"provider-2","chunks":1,
				"message":"held on provider-2","cpu":"1","memory":"1Gi"}]}`))
		case "POST /api/reservation":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			posted = append(posted, body)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cc := NewConsoleClient(srv.URL)

	state, err := cc.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.ManualReservations) != 1 {
		t.Fatalf("manual reservations = %+v", state.ManualReservations)
	}
	r := state.ManualReservations[0]
	if r.Name != "rr-abc" || r.Phase != "Active" || r.Provider != "provider-2" || r.Chunks != 1 {
		t.Errorf("manual reservation decoded wrong: %+v", r)
	}

	if err := cc.ApplyManualReservation(context.Background(), "1", "1Gi"); err != nil {
		t.Fatal(err)
	}
	if err := cc.ReleaseManualReservation(context.Background(), "rr-abc"); err != nil {
		t.Fatal(err)
	}
	if len(posted) != 2 || posted[0]["action"] != "apply" || posted[0]["cpu"] != "1" || posted[0]["memory"] != "1Gi" ||
		posted[1]["action"] != "delete" || posted[1]["name"] != "rr-abc" {
		t.Errorf("reservation actions posted = %+v", posted)
	}
}
