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

package mockgeo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const statusSuccess = "success"

// serve runs the Handler with {ip} bound as a path value (matching the
// GET /json/{ip} route) and decodes the response.
func serve(t *testing.T, ip string) GeoResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/json/"+ip, nil)
	req.SetPathValue("ip", ip)
	rec := httptest.NewRecorder()
	Handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var out GeoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestLongestPrefixMatch(t *testing.T) {
	cases := []struct {
		ip         string
		wantRegion string
		wantCity   string
	}{
		{"172.23.4.10", "QC", "Montreal"},    // .0/26
		{"172.23.4.70", "CA", "San Jose"},    // .64/26
		{"172.23.4.130", "NSW", "Sydney"},    // .128/26
		{"172.23.4.200", "QC", "Montreal"},   // .192/26 reuses .0 city
		{"172.23.6.5", "IDF", "Paris"},       // different /24, different city
		{"172.23.7.130", "MH", "Mumbai"},     // new region
		{"172.23.22.5", "AB", "Stockholm"},   // new region
		{"172.23.22.100", "LOM", "Milan"},    // .64/26
		{"172.23.22.130", "SG", "Singapore"}, // .128/26
	}
	for _, c := range cases {
		got := serve(t, c.ip)
		if got.Status != statusSuccess {
			t.Errorf("%s: status = %q, want success", c.ip, got.Status)
		}
		if got.Query != c.ip {
			t.Errorf("%s: query = %q, want echo", c.ip, got.Query)
		}
		if got.Region != c.wantRegion || got.City != c.wantCity {
			t.Errorf("%s: got %s/%s, want %s/%s", c.ip, got.Region, got.City, c.wantRegion, c.wantCity)
		}
		if got.Lat == 0 && got.Lon == 0 {
			t.Errorf("%s: expected non-zero coordinates for %s", c.ip, c.wantCity)
		}
	}
}

func TestDefaultRow(t *testing.T) {
	// An IP outside every private subnet falls to the 0.0.0.0/0 default: a
	// successful response with no location (empty region, zero coordinates).
	got := serve(t, "8.8.8.8")
	if got.Status != statusSuccess {
		t.Errorf("status = %q, want success", got.Status)
	}
	if got.Region != "" || got.City != "" || got.Lat != 0 || got.Lon != 0 {
		t.Errorf("default row leaked a location: %+v", got)
	}
}

func TestInvalidIP(t *testing.T) {
	got := serve(t, "not-an-ip")
	if got.Status != "fail" {
		t.Errorf("status = %q, want fail", got.Status)
	}
	if got.Message != "invalid query" {
		t.Errorf("message = %q, want invalid query", got.Message)
	}
}

// TestRegionsMatchCarbon guards the invariant that every advertised region code
// is a valid mock-eco key. mock-eco lives in a sibling package, so we assert the
// codes here against the known set to catch drift in review.
func TestRegionsMatchCarbon(t *testing.T) {
	carbonRegions := map[string]bool{
		"QC": true, "LOM": true, "CA": true, "HE": true, "13": true, "NSW": true,
		"IDF": true, "SP": true, "SG": true, "ENG": true, "AB": true, "MH": true,
	}
	for _, r := range compiledTable {
		if r.loc.Region == "" {
			continue // the default row intentionally carries no region
		}
		if !carbonRegions[r.loc.Region] {
			t.Errorf("region %q has no mock-eco carbon profile", r.loc.Region)
		}
	}
}
