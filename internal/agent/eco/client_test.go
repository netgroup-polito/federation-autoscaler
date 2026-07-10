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

package eco

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestForecast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/carbon/forecast" && r.URL.Query().Get("region") == "QC" {
			_ = json.NewEncoder(w).Encode(map[string]any{"region": "QC", "forecast": []int{10, 20, 30}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient()
	ctx := context.Background()

	got, err := c.Forecast(ctx, srv.URL, "QC")
	if err != nil {
		t.Fatalf("Forecast: %v", err)
	}
	if !reflect.DeepEqual(got, []float64{10, 20, 30}) {
		t.Errorf("forecast = %v, want [10 20 30]", got)
	}

	// Disabled (empty baseURL/region) → (nil, nil).
	if v, err := c.Forecast(ctx, "", "QC"); v != nil || err != nil {
		t.Errorf("disabled = (%v,%v), want (nil,nil)", v, err)
	}

	// Unknown region, nothing cached → error (caller falls back to the single value).
	if _, err := c.Forecast(ctx, srv.URL, "ZZ"); err == nil {
		t.Error("expected an error for an uncached failing region")
	}
}
