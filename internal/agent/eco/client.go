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

// Package eco is a tiny, unauthenticated HTTP client for a region→carbon-
// intensity service (mock-eco in the demo, a real ElectricityMaps-style API in
// production). Only the PROVIDER role uses it: a provider looks up its OWN
// region's current carbon intensity and advertises that single number to the
// Broker, which ranks providers by it under the eco placement strategy. Like
// the geo client, it deliberately does NOT use the mTLS Broker client — the
// carbon service is a separate, credential-free endpoint, and the Broker itself
// never dials it.
package eco

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// carbonResponse mirrors the mock-eco GET /carbon?region=XX JSON response.
type carbonResponse struct {
	Region          string  `json:"region"`
	CarbonIntensity float64 `json:"carbonIntensity"`
}

// forecastResponse mirrors the mock-eco GET /carbon/forecast?region=XX response
// (the next hours of carbon intensity, current hour first).
type forecastResponse struct {
	Region   string `json:"region"`
	Forecast []int  `json:"forecast"`
}

type cacheEntry struct {
	value     float64
	fetchedAt time.Time
}

type forecastEntry struct {
	values    []float64
	fetchedAt time.Time
}

// Client fetches a region's current carbon intensity (and its hourly forecast)
// and caches each per region for a TTL (values change at most hourly). Safe for
// concurrent use.
type Client struct {
	mu     sync.Mutex
	cache  map[string]cacheEntry
	fcache map[string]forecastEntry
	ttl    time.Duration
	now    func() time.Time
	http   *http.Client
}

// NewClient returns an eco Client caching for 1 h with a 5 s per-request timeout.
func NewClient() *Client {
	return &Client{
		cache:  make(map[string]cacheEntry),
		fcache: make(map[string]forecastEntry),
		ttl:    time.Hour,
		now:    time.Now,
		http:   &http.Client{Timeout: 5 * time.Second},
	}
}

// CurrentCarbon returns the current grid carbon intensity (gCO2eq/kWh) for
// region from baseURL (e.g. http://mock-eco:8081). It returns (nil, nil) when
// disabled (baseURL or region empty). On a fetch error it falls back to a still-
// cached value if one exists (returning it with a nil error); otherwise it
// returns (nil, err) and the caller advertises no carbon intensity.
func (c *Client) CurrentCarbon(ctx context.Context, baseURL, region string) (*float64, error) {
	if baseURL == "" || region == "" {
		return nil, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.cache[region]; ok && c.now().Sub(e.fetchedAt) < c.ttl {
		v := e.value
		return &v, nil
	}

	reqURL := fmt.Sprintf("%s/carbon?region=%s", baseURL, url.QueryEscape(region))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return c.staleOrErr(region, fmt.Errorf("eco: build request: %w", err))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.staleOrErr(region, fmt.Errorf("eco: call mock-eco: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return c.staleOrErr(region, fmt.Errorf("eco: mock-eco returned status %d for region %q", resp.StatusCode, region))
	}

	var out carbonResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return c.staleOrErr(region, fmt.Errorf("eco: decode response: %w", err))
	}

	c.cache[region] = cacheEntry{value: out.CarbonIntensity, fetchedAt: c.now()}
	v := out.CarbonIntensity
	return &v, nil
}

// staleOrErr returns a still-cached value (with a nil error) when one exists for
// region, otherwise the supplied error. The caller must hold c.mu.
func (c *Client) staleOrErr(region string, err error) (*float64, error) {
	if e, ok := c.cache[region]; ok {
		v := e.value
		return &v, nil
	}
	return nil, err
}

// Forecast returns the region's hourly carbon-intensity forecast (gCO2eq/kWh,
// current hour first) from baseURL. It returns (nil, nil) when disabled (baseURL
// or region empty). On a fetch error it falls back to a still-cached series if one
// exists (nil error); otherwise (nil, err) and the caller advertises no forecast
// (falling back to the single current value). Cached per region for the TTL.
func (c *Client) Forecast(ctx context.Context, baseURL, region string) ([]float64, error) {
	if baseURL == "" || region == "" {
		return nil, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.fcache[region]; ok && c.now().Sub(e.fetchedAt) < c.ttl {
		return append([]float64(nil), e.values...), nil
	}

	reqURL := fmt.Sprintf("%s/carbon/forecast?region=%s", baseURL, url.QueryEscape(region))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return c.staleForecastOrErr(region, fmt.Errorf("eco: build forecast request: %w", err))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.staleForecastOrErr(region, fmt.Errorf("eco: call mock-eco forecast: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return c.staleForecastOrErr(region, fmt.Errorf("eco: mock-eco forecast returned status %d for region %q", resp.StatusCode, region))
	}

	var out forecastResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return c.staleForecastOrErr(region, fmt.Errorf("eco: decode forecast response: %w", err))
	}

	vals := make([]float64, len(out.Forecast))
	for i, v := range out.Forecast {
		vals[i] = float64(v)
	}
	c.fcache[region] = forecastEntry{values: vals, fetchedAt: c.now()}
	return append([]float64(nil), vals...), nil
}

// staleForecastOrErr returns a still-cached forecast (with a nil error) when one
// exists for region, otherwise the supplied error. The caller must hold c.mu.
func (c *Client) staleForecastOrErr(region string, err error) ([]float64, error) {
	if e, ok := c.fcache[region]; ok {
		return append([]float64(nil), e.values...), nil
	}
	return nil, err
}
