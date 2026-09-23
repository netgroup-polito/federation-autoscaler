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

// ConsoleClient talks to the Consumer Agent's console server (plain HTTP,
// NodePort 30445). It drives policy switches and UDP probe requests.
type ConsoleClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewConsoleClient constructs a client pointing at the console's base URL
// (e.g. "http://<nodeIP>:30445").
func NewConsoleClient(baseURL string) *ConsoleClient {
	return &ConsoleClient{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// SetPolicy creates/updates the ConsumerPolicy CRD via POST /api/policy.
// policyType is one of "Random", "Eco", "Latency", "Price", "None" (deletes).
// The agent re-reads the CRD every 15s heartbeat.
func (cc *ConsoleClient) SetPolicy(ctx context.Context, policyType string) error {
	body := map[string]string{"type": policyType}
	return cc.post(ctx, "/api/policy", body)
}

// SetPolicyWithPrompt is SetPolicy for policies that carry a natural-language
// request (ConsumerChoice). It must be used for them rather than SetPolicy: the
// console overwrites spec.userPrompt on every POST, so a type-only POST would
// wipe the prompt instead of leaving it alone.
func (cc *ConsoleClient) SetPolicyWithPrompt(ctx context.Context, policyType, userPrompt string) error {
	body := map[string]string{"type": policyType, "userPrompt": userPrompt}
	return cc.post(ctx, "/api/policy", body)
}

// ConsumerState is the part of the consumer console's GET /api/state that the
// harnesses read. Policy is a plain string on the wire.
type ConsumerState struct {
	ClusterID          string              `json:"clusterId"`
	Policy             string              `json:"policy"`
	UserPrompt         string              `json:"userPrompt,omitempty"`
	Region             string              `json:"region"`
	Location           *ConsumerLocation   `json:"location,omitempty"`
	ManualReservations []ManualReservation `json:"manualReservations"`
}

// ManualReservation is one console-managed ResourceRequest as /api/state lists
// it. Phase is the ResourceRequest phase: Pending, Reserved, Active (the
// borrowed virtual node exists), Migrating or Failed.
type ManualReservation struct {
	Name     string `json:"name"`
	Phase    string `json:"phase,omitempty"`
	Provider string `json:"provider,omitempty"`
	Chunks   int32  `json:"chunks,omitempty"`
	Message  string `json:"message,omitempty"`
}

// ApplyManualReservation asks the consumer for capacity through the console
// (POST /api/reservation, action apply), exactly as an operator would. The
// consumer's manual-reservation controller then picks the provider through the
// agent's local API -- the path the placement policy acts on -- and reserves it.
func (cc *ConsoleClient) ApplyManualReservation(ctx context.Context, cpu, memory string) error {
	body := map[string]string{"action": "apply", "cpu": cpu, "memory": memory}
	return cc.post(ctx, "/api/reservation", body)
}

// ReleaseManualReservation releases a console-managed reservation by name.
func (cc *ConsoleClient) ReleaseManualReservation(ctx context.Context, name string) error {
	body := map[string]string{"action": "delete", "name": name}
	return cc.post(ctx, "/api/reservation", body)
}

// ConsumerLocation is the consumer's auto-discovered location: the same one its
// agent heartbeats to the Broker, and so the one the Broker's distance-based
// logic uses. Lat and Lon are zero when the geo lookup did not succeed.
type ConsumerLocation struct {
	IP     string  `json:"ip,omitempty"`
	Region string  `json:"region,omitempty"`
	City   string  `json:"city,omitempty"`
	Lat    float64 `json:"lat,omitempty"`
	Lon    float64 `json:"lon,omitempty"`
}

// HasCoordinates reports whether the geo lookup produced a usable position.
func (l *ConsumerLocation) HasCoordinates() bool {
	return l != nil && (l.Lat != 0 || l.Lon != 0)
}

// State reads GET /api/state.
func (cc *ConsoleClient) State(ctx context.Context) (*ConsumerState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cc.BaseURL+"/api/state", nil)
	if err != nil {
		return nil, fmt.Errorf("build state request: %w", err)
	}
	resp, err := cc.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /api/state: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET /api/state returned %d: %s", resp.StatusCode, string(body))
	}
	var state ConsumerState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return nil, fmt.Errorf("decode /api/state: %w", err)
	}
	return &state, nil
}

// ProbeRequest is the body sent to POST /api/probe.
type ProbeRequest struct {
	Candidates []ProbeCandidate `json:"candidates"`
}

// ProbeCandidate identifies one provider to UDP-probe.
type ProbeCandidate struct {
	ProviderClusterID string `json:"providerClusterId"`
	Endpoint          string `json:"endpoint"`
}

// ProbeResponse is the response from POST /api/probe.
type ProbeResponse struct {
	Chosen   string             `json:"chosen"`
	RTTs     map[string]float64 `json:"rtts"`
	Duration float64            `json:"durationMs"`
}

// Probe triggers UDP RTT measurement from the Consumer Agent's network
// namespace via POST /api/probe. This endpoint uses the same latency.Prober
// the agent uses for real placement decisions.
func (cc *ConsoleClient) Probe(ctx context.Context, candidates []ProbeCandidate) (*ProbeResponse, error) {
	reqBody := ProbeRequest{Candidates: candidates}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal probe request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cc.BaseURL+"/api/probe", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("build probe request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := cc.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("probe request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("probe returned %d: %s", resp.StatusCode, string(body))
	}

	var result ProbeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode probe response: %w", err)
	}
	return &result, nil
}

// WaitForPolicyPropagation waits for the policy to take effect by polling
// GET /api/state until the response shows the expected policy type or the
// context expires. The agent re-reads the CRD every 15s heartbeat, so this
// typically resolves within ~15s.
func (cc *ConsoleClient) WaitForPolicyPropagation(ctx context.Context, expectedType string) error {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		state, err := cc.getState(ctx)
		if err != nil {
			return fmt.Errorf("GET /api/state: %w", err)
		}
		if currentPolicy, ok := state["policy"]; ok {
			if policyMap, ok := currentPolicy.(map[string]any); ok {
				if t, ok := policyMap["type"]; ok && t == expectedType {
					return nil
				}
			}
		}
		if expectedType == "None" || expectedType == "" {
			if _, ok := state["policy"]; !ok {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for policy %q to propagate", expectedType)
		case <-ticker.C:
		}
	}
}

func (cc *ConsoleClient) getState(ctx context.Context) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cc.BaseURL+"/api/state", nil)
	if err != nil {
		return nil, err
	}
	resp, err := cc.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var state map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return nil, err
	}
	return state, nil
}

func (cc *ConsoleClient) post(ctx context.Context, path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cc.BaseURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := cc.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s returned %d: %s", path, resp.StatusCode, string(respBody))
	}
	return nil
}
