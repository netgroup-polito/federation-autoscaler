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

// Package geo is a tiny, unauthenticated HTTP client for an ip-api.com-style
// geolocation lookup (mock-geo in the demo, a real geo-IP API in production). It
// is used by BOTH agent roles for automatic location discovery: each agent reads
// its own node IP and looks it up, then advertises the discovered coordinates
// (providers on the ClusterAdvertisement, the consumer on its heartbeat) and, on
// the provider, uses the returned region code to key its carbon lookup. It
// deliberately does NOT go through the mTLS Broker client — the geo service is a
// separate, credential-free endpoint.
package geo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Location is a resolved IP location. Lat/Lon feed the latency strategy; Region
// (a code such as "QC") keys the carbon lookup; City/RegionName/CountryCode/
// ContinentCode are informational (dashboards / consoles).
type Location struct {
	Lat           float64
	Lon           float64
	Region        string
	RegionName    string
	City          string
	CountryCode   string
	ContinentCode string
}

// geoResponse mirrors the mock-geo GET /json/{ip} JSON (an ip-api.com subset).
type geoResponse struct {
	Status        string  `json:"status"`
	ContinentCode string  `json:"continentCode"`
	CountryCode   string  `json:"countryCode"`
	Region        string  `json:"region"`
	RegionName    string  `json:"regionName"`
	City          string  `json:"city"`
	Lat           float64 `json:"lat"`
	Lon           float64 `json:"lon"`
}

// Client resolves IPs to locations and caches them permanently per IP (an IP's
// location is static for a cluster's lifetime). Safe for concurrent use.
type Client struct {
	mu    sync.Mutex
	cache map[string]Location
	http  *http.Client
}

// NewClient returns a geo Client with a 5 s per-request timeout.
func NewClient() *Client {
	return &Client{
		cache: make(map[string]Location),
		http:  &http.Client{Timeout: 5 * time.Second},
	}
}

// Lookup returns the location of ip from baseURL (e.g. http://mock-geo:8080),
// caching the result. ok is false with a nil error when the lookup is disabled
// (baseURL or ip empty) or the service returned no usable location (a "fail"
// envelope, or a default row with no region and zero coordinates). A non-nil
// error means the service was configured but unreachable/invalid; the caller
// should proceed without a location.
func (c *Client) Lookup(ctx context.Context, baseURL, ip string) (Location, bool, error) {
	if baseURL == "" || ip == "" {
		return Location{}, false, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.cache[ip]; ok {
		return v, true, nil
	}

	reqURL := fmt.Sprintf("%s/json/%s", baseURL, url.PathEscape(ip))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return Location{}, false, fmt.Errorf("geo: build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Location{}, false, fmt.Errorf("geo: call mock-geo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Location{}, false, fmt.Errorf("geo: mock-geo returned status %d for ip %q", resp.StatusCode, ip)
	}

	var out geoResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Location{}, false, fmt.Errorf("geo: decode response: %w", err)
	}

	// A "fail" envelope or a location with neither a region nor coordinates
	// (the default row) carries nothing worth advertising.
	if out.Status == "fail" || (out.Region == "" && out.Lat == 0 && out.Lon == 0) {
		return Location{}, false, nil
	}

	v := Location{
		Lat:           out.Lat,
		Lon:           out.Lon,
		Region:        out.Region,
		RegionName:    out.RegionName,
		City:          out.City,
		CountryCode:   out.CountryCode,
		ContinentCode: out.ContinentCode,
	}
	c.cache[ip] = v
	return v, true, nil
}
