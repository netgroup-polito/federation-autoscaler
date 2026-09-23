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

// Package ollama is a lightweight HTTP client for a local Ollama instance
// (https://ollama.ai) used by the ConsumerChoice placement strategy. It sends
// a structured provider list and a natural-language user request to the LLM,
// which returns a ranking of the candidate provider IDs as a JSON object.
//
// It is used by the consumer role's localapi server when the active
// ConsumerPolicy has placement type "ConsumerChoice", and by the ConsumerChoice
// end-to-end suite (federation-tests/consumerchoice), which drives the same selector so
// that what it validates is the shipped decision logic rather than a copy. It
// never contacts the Broker or any external service -- only the Ollama
// endpoint it is given.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// defaultTimeout is the per-request timeout when none is configured.
// Generous on purpose: a small model on CPU needs tens of seconds to rank a
// handful of providers, and a call that times out falls back to the
// deterministic strategy instead of using the model at all.
const defaultTimeout = 120 * time.Second

// jsonMode is the `format` sent to Ollama: plain JSON mode, well-formed JSON of
// any shape. The model is asked for the selection object in the system prompt;
// nothing else constrains or tunes its generation (sampling stays at the
// server's defaults), so the model decides on its own.
const jsonMode = "json"

// Options tunes a Client. The zero value is the agent's historical behaviour
// (120 s timeout, no consumer location).
type Options struct {
	// Timeout bounds one /api/generate call. Zero means 120 s.
	Timeout time.Duration
	// ConsumerLocation, when known, is included in the prompt so the model can
	// judge proximity from the raw coordinates itself. Nothing is derived from
	// it on the model's behalf.
	ConsumerLocation *Location
}

// Client calls a local Ollama instance to select a provider. Safe for
// concurrent use; each Select call is independent.
type Client struct {
	baseURL string        // e.g. "http://localhost:11434"
	model   string        // e.g. "llama3.2"
	timeout time.Duration // per-request timeout for the /api/generate call
	opts    Options
	http    *http.Client
}

// New returns an Ollama Client. baseURL is the Ollama API root (e.g.
// "http://localhost:11434"), model is the model name (e.g. "llama3.2").
func New(baseURL, model string) *Client {
	return NewWithOptions(baseURL, model, Options{})
}

// NewWithOptions is New with explicit Options.
func NewWithOptions(baseURL, model string, opts Options) *Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		timeout: timeout,
		opts:    opts,
		http:    &http.Client{Timeout: timeout},
	}
}

// Timeout is the upper bound on one LLM call. A caller that runs the call under
// its own context should give it at least this long, or the call is cut short
// before the configured timeout.
func (c *Client) Timeout() time.Duration { return c.timeout }

// WithConsumerLocation returns a copy of the client that sends loc as the
// consumer location, for a caller that only learns where it is at run time.
// The receiver is not modified, so one shared client stays safe to use
// concurrently.
func (c *Client) WithConsumerLocation(loc *Location) *Client {
	cp := *c
	cp.opts.ConsumerLocation = loc
	return &cp
}

// ollamaRequest is the JSON body sent to POST /api/generate.
type ollamaRequest struct {
	Model  string `json:"model"`
	System string `json:"system"`
	Prompt string `json:"prompt"`
	Format string `json:"format"`
	Stream bool   `json:"stream"`
}

// ollamaResponse is the JSON body returned by POST /api/generate (non-streaming).
// Beyond the generated text it carries the runtime's own timing, which is the
// honest source for how long the model itself took versus the HTTP round trip.
type ollamaResponse struct {
	Model              string    `json:"model,omitempty"`
	CreatedAt          time.Time `json:"created_at,omitzero"`
	Response           string    `json:"response"`
	Done               bool      `json:"done"`
	DoneReason         string    `json:"done_reason,omitempty"`
	TotalDuration      int64     `json:"total_duration,omitempty"`
	LoadDuration       int64     `json:"load_duration,omitempty"`
	PromptEvalCount    int       `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64     `json:"prompt_eval_duration,omitempty"`
	EvalCount          int       `json:"eval_count,omitempty"`
	EvalDuration       int64     `json:"eval_duration,omitempty"`
}

// ErrorKind classifies why a selection failed, so a caller can count failures
// by cause instead of pattern-matching error strings.
type ErrorKind string

const (
	ErrKindNone              ErrorKind = ""
	ErrKindNoCapacity        ErrorKind = "no_capacity"
	ErrKindUnreachable       ErrorKind = "unreachable"
	ErrKindTimeout           ErrorKind = "timeout"
	ErrKindHTTPStatus        ErrorKind = "http_status"
	ErrKindEnvelopeDecode    ErrorKind = "envelope_decode"
	ErrKindInvalidJSON       ErrorKind = "invalid_json"
	ErrKindEmptyProviderID   ErrorKind = "empty_provider_id"
	ErrKindUnknownProviderID ErrorKind = "unknown_provider_id"
)

// SelectError is the error Select and SelectDetailed return: the original
// message, plus the Kind it falls under.
type SelectError struct {
	Kind ErrorKind
	Err  error
}

func (e *SelectError) Error() string { return e.Err.Error() }
func (e *SelectError) Unwrap() error { return e.Err }

func selectErr(kind ErrorKind, format string, args ...any) *SelectError {
	return &SelectError{Kind: kind, Err: fmt.Errorf(format, args...)}
}

// Trace is the full record of one selection: what was sent, what came back,
// when, and how it was judged. SelectDetailed returns it even when the
// selection fails, because a failed decision is exactly the one worth
// inspecting afterwards.
type Trace struct {
	Model        string             `json:"model"`
	SystemPrompt string             `json:"systemPrompt"`
	UserPrompt   string             `json:"userPrompt"`
	Providers    []ProviderInfo     `json:"providers"`
	Format       string             `json:"format,omitempty"`
	StartedAt    time.Time          `json:"startedAt"`
	FinishedAt   time.Time          `json:"finishedAt"`
	HTTPStatus   int                `json:"httpStatus,omitempty"`
	RawResponse  string             `json:"rawResponse,omitempty"`
	RawErrorBody string             `json:"rawErrorBody,omitempty"`
	Envelope     *ollamaResponse    `json:"envelope,omitempty"`
	Parsed       *SelectionResponse `json:"parsed,omitempty"`
	// Ranked is the model's ranking with unknown IDs removed, best first --
	// what the caller acts on. UnknownIDs are the ones removed.
	Ranked     []string `json:"ranked,omitempty"`
	UnknownIDs []string `json:"unknownIds,omitempty"`
	// SingleCandidate is true when only one provider had capacity and the
	// model was therefore not called at all.
	SingleCandidate bool      `json:"singleCandidate"`
	ErrorKind       ErrorKind `json:"errorKind,omitempty"`
	Error           string    `json:"error,omitempty"`
}

// Duration is how long the selection took end to end.
func (t *Trace) Duration() time.Duration { return t.FinishedAt.Sub(t.StartedAt) }

func (t *Trace) fail(err *SelectError) (*Trace, error) {
	t.FinishedAt = time.Now()
	t.ErrorKind = err.Kind
	t.Error = err.Error()
	return t, err
}

// Select sends the provider list and user prompt to Ollama and returns a
// ranked list of provider ClusterIDs ordered by preference (best first).
// Returns (nil, err) on failure; the caller MUST fall back to a
// deterministic strategy when err != nil.
func (c *Client) Select(ctx context.Context, userPrompt string, nodeGroups []brokerapi.NodeGroupView) ([]string, error) {
	trace, err := c.SelectDetailed(ctx, userPrompt, nodeGroups)
	if err != nil {
		return nil, err
	}
	return trace.Ranked, nil
}

// SelectDetailed is Select, returning the full Trace alongside the result.
// The returned Trace is never nil. Its decision rules are exactly Select's.
func (c *Client) SelectDetailed(ctx context.Context, userPrompt string, nodeGroups []brokerapi.NodeGroupView) (*Trace, error) {
	trace := &Trace{Model: c.model, SystemPrompt: SystemPrompt, StartedAt: time.Now()}

	// Build provider info list (only providers with available capacity).
	validIDs := make(map[string]struct{})
	for _, ng := range nodeGroups {
		if ng.MaxSize > ng.CurrentReserved {
			trace.Providers = append(trace.Providers, NodeGroupViewToProviderInfo(ng))
			validIDs[ng.ProviderClusterID] = struct{}{}
		}
	}
	if len(trace.Providers) == 0 {
		return trace.fail(selectErr(ErrKindNoCapacity, "no providers with available capacity"))
	}

	// If there is only one provider, skip the AI call entirely.
	if len(trace.Providers) == 1 {
		trace.SingleCandidate = true
		trace.Ranked = []string{trace.Providers[0].ProviderID}
		trace.FinishedAt = time.Now()
		return trace, nil
	}

	trace.UserPrompt = BuildUserPrompt(userPrompt, c.opts.ConsumerLocation, trace.Providers)
	trace.Format = jsonMode

	envelope, err := c.generate(ctx, trace)
	if err != nil {
		return trace.fail(err)
	}
	trace.Envelope = envelope
	trace.RawResponse = envelope.Response

	// Parse the LLM's JSON output.
	var selection SelectionResponse
	if err := json.Unmarshal([]byte(envelope.Response), &selection); err != nil {
		return trace.fail(selectErr(ErrKindInvalidJSON, "parse LLM selection JSON %q: %w", envelope.Response, err))
	}
	trace.Parsed = &selection

	// Prefer RankedList; fall back to single ProviderID for backward compat.
	if len(selection.RankedList) > 0 {
		// A model caught in a repetition loop lists the same IDs over and over;
		// the first occurrence is its ranking, the rest adds nothing.
		ranked := make(map[string]bool, len(validIDs))
		for _, id := range selection.RankedList {
			switch _, ok := validIDs[id]; {
			case !ok:
				trace.UnknownIDs = append(trace.UnknownIDs, id)
			case !ranked[id]:
				ranked[id] = true
				trace.Ranked = append(trace.Ranked, id)
			}
		}
		if len(trace.Ranked) == 0 {
			return trace.fail(selectErr(ErrKindUnknownProviderID,
				"LLM rankedList contains no valid IDs (valid: %v)", validIDsList(validIDs)))
		}
		trace.FinishedAt = time.Now()
		return trace, nil
	}

	if selection.ProviderID == "" {
		return trace.fail(selectErr(ErrKindEmptyProviderID,
			"LLM returned empty providerId in response %q", envelope.Response))
	}

	// Validate the returned ID exists in the input list.
	if _, ok := validIDs[selection.ProviderID]; !ok {
		trace.UnknownIDs = []string{selection.ProviderID}
		return trace.fail(selectErr(ErrKindUnknownProviderID,
			"LLM returned unknown providerId %q (valid: %v)", selection.ProviderID, validIDsList(validIDs)))
	}

	trace.Ranked = []string{selection.ProviderID}
	trace.FinishedAt = time.Now()
	return trace, nil
}

// generate performs the /api/generate round trip for a prepared trace.
func (c *Client) generate(ctx context.Context, trace *Trace) (*ollamaResponse, *SelectError) {
	reqBody := ollamaRequest{
		Model:  c.model,
		System: trace.SystemPrompt,
		Prompt: trace.UserPrompt,
		Format: trace.Format,
		Stream: false,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, selectErr(ErrKindInvalidJSON, "marshal ollama request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		c.baseURL+"/api/generate", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, selectErr(ErrKindUnreachable, "build ollama HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		if isTimeout(err) {
			return nil, selectErr(ErrKindTimeout, "call ollama: %w", err)
		}
		return nil, selectErr(ErrKindUnreachable, "call ollama: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	trace.HTTPStatus = resp.StatusCode

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		trace.RawErrorBody = string(body)
		return nil, selectErr(ErrKindHTTPStatus, "ollama returned status %d: %s", resp.StatusCode, string(body))
	}

	var envelope ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		if isTimeout(err) {
			return nil, selectErr(ErrKindTimeout, "read ollama response: %w", err)
		}
		return nil, selectErr(ErrKindEnvelopeDecode, "decode ollama response: %w", err)
	}
	return &envelope, nil
}

// isTimeout reports whether err is a deadline being hit, from either the
// request context or the http.Client timeout (which surfaces as a net.Error).
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// validIDsList returns a sorted list of valid IDs for error messages.
func validIDsList(ids map[string]struct{}) []string {
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// DeterministicFallback returns a ranked list of providers using a
// deterministic strategy when the AI is unavailable or returns an invalid
// choice. Priority per provider:
//  1. Lowest cost (if priced)
//  2. Lowest carbon intensity (if advertised)
//  3. Most available chunks
//
// Returns (nil, false) if no provider has available capacity.
func DeterministicFallback(nodeGroups []brokerapi.NodeGroupView) ([]string, bool) {
	type candidate struct {
		id        string
		cost      float64
		hasCost   bool
		carbon    float64
		hasCarbon bool
		available int32
	}

	candidates := make([]candidate, 0, len(nodeGroups))
	for _, ng := range nodeGroups {
		avail := ng.MaxSize - ng.CurrentReserved
		if avail <= 0 {
			continue
		}
		c := candidate{
			id:        ng.ProviderClusterID,
			available: avail,
		}
		if ng.Cost != nil {
			c.cost = ng.Cost.AsApproximateFloat64()
			c.hasCost = true
		}
		if ng.CarbonIntensity != nil {
			c.carbon = *ng.CarbonIntensity
			c.hasCarbon = true
		}
		candidates = append(candidates, c)
	}

	if len(candidates) == 0 {
		return nil, false
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		ci, cj := candidates[i], candidates[j]
		// 1. Prefer priced over unpriced, then lowest cost.
		if ci.hasCost != cj.hasCost {
			return ci.hasCost
		}
		if ci.hasCost && cj.hasCost && ci.cost != cj.cost {
			return ci.cost < cj.cost
		}
		// 2. Prefer carbon-advertised, then lowest carbon.
		if ci.hasCarbon != cj.hasCarbon {
			return ci.hasCarbon
		}
		if ci.hasCarbon && cj.hasCarbon && ci.carbon != cj.carbon {
			return ci.carbon < cj.carbon
		}
		// 3. Most available chunks.
		return ci.available > cj.available
	})

	ranked := make([]string, len(candidates))
	for i, c := range candidates {
		ranked[i] = c.id
	}
	return ranked, true
}
