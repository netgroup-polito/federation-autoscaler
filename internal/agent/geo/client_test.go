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

package geo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLookupDecodesLocation(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"172.23.4.10","status":"success","continentCode":"NA","countryCode":"CA","region":"QC","regionName":"Quebec","city":"Montreal","lat":45.6085,"lon":-73.5493}`))
	}))
	defer srv.Close()

	c := NewClient()
	loc, ok, err := c.Lookup(context.Background(), srv.URL, "172.23.4.10")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if gotPath != "/json/172.23.4.10" {
		t.Errorf("path = %q, want /json/172.23.4.10", gotPath)
	}
	if loc.Region != "QC" || loc.City != "Montreal" {
		t.Errorf("got %s/%s, want QC/Montreal", loc.Region, loc.City)
	}
	if loc.Lat != 45.6085 || loc.Lon != -73.5493 {
		t.Errorf("coords = %v,%v", loc.Lat, loc.Lon)
	}
}

func TestLookupDisabled(t *testing.T) {
	c := NewClient()
	if _, ok, err := c.Lookup(context.Background(), "", "1.2.3.4"); ok || err != nil {
		t.Errorf("empty baseURL: ok=%v err=%v, want false/nil", ok, err)
	}
	if _, ok, err := c.Lookup(context.Background(), "http://x", ""); ok || err != nil {
		t.Errorf("empty ip: ok=%v err=%v, want false/nil", ok, err)
	}
}

func TestLookupNoLocation(t *testing.T) {
	// A default-row / fail response carries nothing worth advertising → ok=false.
	for _, body := range []string{
		`{"query":"8.8.8.8","status":"success"}`,
		`{"query":"bad","status":"fail","message":"invalid query"}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		c := NewClient()
		_, ok, err := c.Lookup(context.Background(), srv.URL, "8.8.8.8")
		srv.Close()
		if ok || err != nil {
			t.Errorf("body %q: ok=%v err=%v, want false/nil", body, ok, err)
		}
	}
}

func TestLookupCachesByIP(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"status":"success","region":"QC","city":"Montreal","lat":45.6,"lon":-73.5}`))
	}))
	defer srv.Close()

	c := NewClient()
	for i := range 3 {
		if _, ok, err := c.Lookup(context.Background(), srv.URL, "172.23.4.10"); !ok || err != nil {
			t.Fatalf("lookup %d: ok=%v err=%v", i, ok, err)
		}
	}
	if hits != 1 {
		t.Errorf("server hits = %d, want 1 (cached)", hits)
	}
}

func TestLookupServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient()
	_, ok, err := c.Lookup(context.Background(), srv.URL, "172.23.4.10")
	if ok {
		t.Error("ok = true, want false on 500")
	}
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Errorf("err = %v, want status 500", err)
	}
}
