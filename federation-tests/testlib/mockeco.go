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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MockEcoClient talks to the controllable mock-eco's admin API.
type MockEcoClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewMockEcoClient constructs a client for the controllable mock-eco service.
func NewMockEcoClient(baseURL string) *MockEcoClient {
	return &MockEcoClient{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// SetCarbon sets the carbon intensity for a region. The forecast is optional;
// when nil, the service generates a flat forecast from the intensity value.
func (c *MockEcoClient) SetCarbon(ctx context.Context, region string, intensity int, forecast []int) error {
	body := map[string]any{
		"region":          region,
		"carbonIntensity": intensity,
	}
	if forecast != nil {
		body["forecast"] = forecast
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/admin/carbon", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("set carbon returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// ResetCarbon clears all overrides, reverting to the static defaults.
func (c *MockEcoClient) ResetCarbon(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+"/admin/carbon", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("reset carbon returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// CheckHealthy verifies the mock-eco service is up.
func (c *MockEcoClient) CheckHealthy(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}
