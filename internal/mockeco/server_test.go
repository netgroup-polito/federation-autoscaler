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

package mockeco

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestForecastHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	ForecastHandler(rec, httptest.NewRequest(http.MethodGet, "/carbon/forecast?region=QC", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var out forecastResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Forecast) != 24 {
		t.Fatalf("forecast length = %d, want 24", len(out.Forecast))
	}
	// The first hour is the region's current-hour value (the series wraps forward).
	if want := regionData["QC"][time.Now().UTC().Hour()%24]; out.Forecast[0] != want {
		t.Errorf("forecast[0] = %d, want current-hour %d", out.Forecast[0], want)
	}
}

func TestForecastHandlerErrors(t *testing.T) {
	unknown := httptest.NewRecorder()
	ForecastHandler(unknown, httptest.NewRequest(http.MethodGet, "/carbon/forecast?region=ZZ", nil))
	if unknown.Code != http.StatusNotFound {
		t.Errorf("unknown region code = %d, want 404", unknown.Code)
	}
	missing := httptest.NewRecorder()
	ForecastHandler(missing, httptest.NewRequest(http.MethodGet, "/carbon/forecast", nil))
	if missing.Code != http.StatusBadRequest {
		t.Errorf("missing region code = %d, want 400", missing.Code)
	}
}
