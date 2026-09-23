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

package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// milan is the consumer location used across these tests.
var milan = &Location{Latitude: 45.4642, Longitude: 9.1900, Region: "LOM"}

func twoCandidates() []brokerapi.NodeGroupView {
	return []brokerapi.NodeGroupView{
		makeNodeGroup("provider-1", 5, 2, floatPtr(0.10), floatPtr(100), "IDF"),
		makeNodeGroup("provider-2", 5, 1, floatPtr(0.05), floatPtr(40), "IDF"),
	}
}

// captureServer answers every /api/generate with envelope and hands the
// decoded request body to the test.
func captureServer(t *testing.T, envelope ollamaResponse) (*httptest.Server, *map[string]json.RawMessage) {
	t.Helper()
	var body map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	t.Cleanup(srv.Close)
	return srv, &body
}

func okEnvelope(response string) ollamaResponse {
	return ollamaResponse{Response: response, Done: true}
}

// The model decides on its own: the request is plain JSON mode with the
// server's default sampling -- no schema, no temperature, no seed -- whatever
// Options are set.
func TestRequestShape_PlainJSONModeOnly(t *testing.T) {
	for name, c := range map[string]func(url string) *Client{
		"New":         func(url string) *Client { return New(url, "m") },
		"WithOptions": func(url string) *Client { return NewWithOptions(url, "m", Options{Timeout: time.Minute}) },
		"WithLocation": func(url string) *Client {
			return NewWithOptions(url, "m", Options{}).WithConsumerLocation(milan)
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv, body := captureServer(t, okEnvelope(`{"providerId":"provider-2"}`))
			if _, err := c(srv.URL).Select(context.Background(), "any", twoCandidates()); err != nil {
				t.Fatal(err)
			}
			if got := string((*body)["format"]); got != `"json"` {
				t.Errorf("format = %s, want plain JSON mode", got)
			}
			for key := range *body {
				switch key {
				case "model", "system", "prompt", "format", "stream":
				default:
					t.Errorf("unexpected request field %q: only the model, prompts and JSON mode are sent", key)
				}
			}
		})
	}
}

func TestNewWithOptions_Timeout(t *testing.T) {
	if got := New("http://x", "m").Timeout(); got != 120*time.Second {
		t.Errorf("default timeout = %s, want 120s (enough for a small model on CPU)", got)
	}
	if got := NewWithOptions("http://x", "m", Options{Timeout: 5 * time.Minute}).Timeout(); got != 5*time.Minute {
		t.Errorf("timeout = %s, want the configured 5m", got)
	}
	located := NewWithOptions("http://x", "m", Options{Timeout: 5 * time.Minute}).WithConsumerLocation(milan)
	if got := located.Timeout(); got != 5*time.Minute {
		t.Errorf("a located copy must keep the timeout; got %s", got)
	}
}

func TestNodeGroupViewToProviderInfo_CapacityAndIdentity(t *testing.T) {
	ng := makeNodeGroup("provider-1", 5, 2, nil, nil, "IDF") // 3 chunks of 2 CPU / 4Gi free

	info := NodeGroupViewToProviderInfo(ng)
	if info.AvailableCPUMillicores != 6000 || info.AvailableMemoryMiB != 12288 {
		t.Errorf("available capacity = %d m / %d MiB, want 6000 / 12288",
			info.AvailableCPUMillicores, info.AvailableMemoryMiB)
	}
	if info.NodeGroupID != ng.ID {
		t.Errorf("nodeGroupId = %q, want %q", info.NodeGroupID, ng.ID)
	}
}

// The model reads the cost as text: 52m must reach it as 0.052, not as the
// 0.052000000000000005 a float conversion yields.
func TestNodeGroupViewToProviderInfo_CostIsExactDecimal(t *testing.T) {
	for milli, want := range map[int64]string{52: `"costPerChunk":0.052}`, 26: `"costPerChunk":0.026}`, 70: `"costPerChunk":0.07}`} {
		ng := makeNodeGroup("provider-1", 5, 0, nil, nil, "")
		ng.Cost = resource.NewMilliQuantity(milli, resource.DecimalSI)
		before := ng.Cost.String()

		body, err := json.Marshal(NodeGroupViewToProviderInfo(ng))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("cost %dm: want %s in %s", milli, want, body)
		}
		if after := ng.Cost.String(); after != before {
			t.Errorf("the Broker's quantity must be left as it was: %s, now %s", before, after)
		}
	}
}

// The model gets raw data and judges proximity itself: the consumer's own
// location goes into the prompt, and nothing is derived from it on the model's
// behalf -- no distance, no ranking.
func TestBuildUserPrompt_ConsumerLocation(t *testing.T) {
	providers := []ProviderInfo{NodeGroupViewToProviderInfo(makeNodeGroup("provider-1", 5, 0, nil, nil, "IDF"))}

	with := BuildUserPrompt("close to me", &Location{Latitude: 45.4642, Longitude: 9.19, Region: "LOM"}, providers)
	for _, want := range []string{
		"CONSUMER LOCATION",
		`{"latitude":45.4642,"longitude":9.19,"region":"LOM"}`,
		`"latitude": 48.85`, // the provider's own coordinates stay as they are
	} {
		if !strings.Contains(with, want) {
			t.Errorf("prompt with a location must contain %s:\n%s", want, with)
		}
	}
	if strings.Contains(strings.ToLower(with), "distance") {
		t.Errorf("no distance may be computed for the model:\n%s", with)
	}
	if strings.Index(with, "CONSUMER LOCATION") > strings.Index(with, "AVAILABLE PROVIDERS") {
		t.Error("the consumer location must come before the provider list")
	}

	if without := BuildUserPrompt("close to me", nil, providers); strings.Contains(without, "CONSUMER LOCATION") {
		t.Errorf("an unknown location must be left out, not sent empty:\n%s", without)
	}
}

// The model writes what it ranks by before it ranks: the request's aim, then
// the values copied from the list, then the ranking. Written the other way
// round, llama3.2 ranked a "greenest" request by cost every time.
func TestSystemPrompt_ValuesBeforeRanking(t *testing.T) {
	schema := SystemPrompt[strings.Index(SystemPrompt, "schema: {"):]
	reason, values, ranked := strings.Index(schema, `"reason"`), strings.Index(schema, `"values"`),
		strings.Index(schema, `"rankedList"`)
	if reason < 0 || values < reason || ranked < values {
		t.Errorf("the schema must list reason, values, rankedList in this order:\n%s", schema)
	}

	start, end := strings.Index(SystemPrompt, "How words in the request map to fields:"), strings.Index(SystemPrompt, "Rules:")
	if start < 0 || end < start {
		t.Fatalf("the prompt must map request words to fields before the rules:\n%s", SystemPrompt)
	}
	for _, field := range []string{"carbonIntensity", "costPerChunk", "availableChunks", "latitude/longitude"} {
		if !strings.Contains(SystemPrompt[start:end], field) {
			t.Errorf("no request words map to %s", field)
		}
	}

	// Which end of each value is better, proximity included: a model told only
	// where a provider is ranks a "close to me" request by something else.
	for _, meaning := range []string{"LOWEST carbonIntensity", "LOWEST costPerChunk", "the farther away, the worse"} {
		if !strings.Contains(SystemPrompt[:start], meaning) {
			t.Errorf("the field glossary does not say %q", meaning)
		}
	}

	// Proximity is the one value the model has to compute, so the prompt gives
	// the rule and asks for the result per provider, like every other value.
	rule := SystemPrompt[strings.Index(SystemPrompt, "When the request is about proximity"):]
	for _, part := range []string{
		"proximityScore = |provider latitude - consumer latitude| + |provider longitude - consumer longitude|",
		`write each provider's proximityScore in "values"`,
		"rank by proximityScore, smallest first",
	} {
		if !strings.Contains(rule, part) {
			t.Errorf("the proximity rule does not say %q", part)
		}
	}
}

// The values the model copies before ranking are its working: an answer that
// carries them ranks exactly like one that does not.
func TestSelect_AnswerWithValuesBeforeTheRanking(t *testing.T) {
	srv, _ := captureServer(t, okEnvelope(`{"reason": "greenest", "values": [{"providerId": "provider-2", `+
		`"carbonIntensity": 40}, {"providerId": "provider-1", "carbonIntensity": 100}], `+
		`"rankedList": ["provider-2", "provider-1"], "providerId": "provider-2", "confidence": 0.9}`))

	ranked, err := New(srv.URL, "m").Select(context.Background(), "greenest", twoCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if len(ranked) != 2 || ranked[0] != "provider-2" || ranked[1] != "provider-1" {
		t.Errorf("ranked = %v, want [provider-2 provider-1]", ranked)
	}
}

// The provider list is long; the request must also be the last thing the model
// reads before it answers, and it must stay the only thing added: no data.
func TestBuildUserPrompt_RequestRepeatedAfterProviders(t *testing.T) {
	providers := []ProviderInfo{NodeGroupViewToProviderInfo(makeNodeGroup("provider-1", 5, 0, nil, nil, "IDF"))}
	prompt := BuildUserPrompt("Prioritize the greenest provider.", milan, providers)

	request := `"Prioritize the greenest provider."`
	providersAt := strings.Index(prompt, "AVAILABLE PROVIDERS")
	if first := strings.Index(prompt, request); first < 0 || first > providersAt {
		t.Errorf("the request must open the prompt:\n%s", prompt)
	}
	if !strings.HasSuffix(prompt, "\n\nRank ALL the providers above for this USER REQUEST: "+request) {
		t.Errorf("the request must be repeated after the provider list:\n%s", prompt)
	}
	if n := strings.Count(prompt, request); n != 2 {
		t.Errorf("the request appears %d times, want 2", n)
	}
}

func TestWithConsumerLocation_CopiesTheClient(t *testing.T) {
	base := NewWithOptions("http://ollama", "m", Options{Timeout: 2 * time.Minute})
	located := base.WithConsumerLocation(milan)

	if base.opts.ConsumerLocation != nil {
		t.Error("WithConsumerLocation must not modify the shared client")
	}
	if located.opts.ConsumerLocation != milan || located.timeout != 2*time.Minute || located.model != "m" {
		t.Errorf("the copy must keep every other setting and carry the location: %+v", located.opts)
	}
}

func TestSelectDetailed_CapturesTrace(t *testing.T) {
	envelope := ollamaResponse{
		Model:         "m",
		Response:      `{"rankedList":["provider-2","provider-1"],"providerId":"provider-2","reason":"greener","confidence":0.8}`,
		Done:          true,
		TotalDuration: 1_500_000_000,
		EvalCount:     37,
	}
	srv, _ := captureServer(t, envelope)

	c := NewWithOptions(srv.URL, "m", Options{ConsumerLocation: milan})
	trace, err := c.SelectDetailed(context.Background(), "the greenest please", twoCandidates())
	if err != nil {
		t.Fatal(err)
	}

	if trace.RawResponse != envelope.Response {
		t.Errorf("raw response not preserved verbatim: %q", trace.RawResponse)
	}
	if trace.Parsed == nil || trace.Parsed.Reason != "greener" || trace.Parsed.Confidence == nil || *trace.Parsed.Confidence != 0.8 {
		t.Errorf("reason/confidence not parsed: %+v", trace.Parsed)
	}
	if trace.Envelope == nil || trace.Envelope.EvalCount != 37 || trace.Envelope.TotalDuration != 1_500_000_000 {
		t.Errorf("runtime metadata not captured: %+v", trace.Envelope)
	}
	if strings.Join(trace.Ranked, ",") != "provider-2,provider-1" {
		t.Errorf("ranked = %v", trace.Ranked)
	}
	if len(trace.Providers) != 2 {
		t.Errorf("both candidates must be sent: %+v", trace.Providers)
	}
	if !strings.Contains(trace.UserPrompt, "the greenest please") || !strings.Contains(trace.UserPrompt, "CONSUMER LOCATION") {
		t.Errorf("user prompt must carry the request and the consumer location:\n%s", trace.UserPrompt)
	}
	if trace.FinishedAt.Before(trace.StartedAt) || trace.ErrorKind != ErrKindNone {
		t.Errorf("bad timing or error on success: %+v", trace)
	}
}

func TestSelectDetailed_ErrorKinds(t *testing.T) {
	respond := func(status int, response string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			if status != http.StatusOK {
				http.Error(w, "model busy", status)
				return
			}
			_ = json.NewEncoder(w).Encode(okEnvelope(response))
		}
	}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		timeout time.Duration
		want    ErrorKind
	}{
		{"not JSON", respond(http.StatusOK, "sure, provider-2"), 0, ErrKindInvalidJSON},
		{"unknown ID", respond(http.StatusOK, `{"providerId":"provider-9"}`), 0, ErrKindUnknownProviderID},
		{"unknown IDs only in ranking", respond(http.StatusOK, `{"rankedList":["x","y"]}`), 0, ErrKindUnknownProviderID},
		{"empty ID", respond(http.StatusOK, `{"providerId":""}`), 0, ErrKindEmptyProviderID},
		{"HTTP error", respond(http.StatusServiceUnavailable, ""), 0, ErrKindHTTPStatus},
		{"timeout", func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(300 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(okEnvelope(`{"providerId":"provider-1"}`))
		}, 50 * time.Millisecond, ErrKindTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			trace, err := NewWithOptions(srv.URL, "m", Options{Timeout: tc.timeout}).
				SelectDetailed(context.Background(), "any", twoCandidates())
			var selErr *SelectError
			if !errors.As(err, &selErr) || selErr.Kind != tc.want {
				t.Fatalf("error = %v, want kind %q", err, tc.want)
			}
			if trace == nil || trace.ErrorKind != tc.want || trace.Error == "" {
				t.Errorf("trace must record the failure: %+v", trace)
			}
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close() // nothing listens there any more

		_, err := New(url, "m").SelectDetailed(context.Background(), "any", twoCandidates())
		var selErr *SelectError
		if !errors.As(err, &selErr) || selErr.Kind != ErrKindUnreachable {
			t.Fatalf("error = %v, want kind unreachable", err)
		}
	})
}

func TestSelectDetailed_RecordsUnknownIDsDroppedFromRanking(t *testing.T) {
	srv, _ := captureServer(t, okEnvelope(`{"rankedList":["bogus","provider-1","provider-2"],"providerId":"bogus"}`))

	trace, err := New(srv.URL, "m").SelectDetailed(context.Background(), "any", twoCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(trace.UnknownIDs, ",") != "bogus" {
		t.Errorf("unknown IDs = %v, want [bogus]", trace.UnknownIDs)
	}
	if strings.Join(trace.Ranked, ",") != "provider-1,provider-2" {
		t.Errorf("ranked = %v, want the valid IDs only", trace.Ranked)
	}
}

func TestSelectDetailed_ShortcutsWithoutCallingTheModel(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(okEnvelope(`{"providerId":"provider-1"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "m")

	single := []brokerapi.NodeGroupView{makeNodeGroup("provider-1", 5, 0, nil, nil, "")}
	trace, err := c.SelectDetailed(context.Background(), "any", single)
	if err != nil || !trace.SingleCandidate || strings.Join(trace.Ranked, ",") != "provider-1" {
		t.Errorf("single candidate: trace=%+v err=%v", trace, err)
	}

	full := []brokerapi.NodeGroupView{makeNodeGroup("provider-1", 5, 5, nil, nil, "")}
	_, err = c.SelectDetailed(context.Background(), "any", full)
	var selErr *SelectError
	if !errors.As(err, &selErr) || selErr.Kind != ErrKindNoCapacity {
		t.Errorf("no capacity: error = %v, want kind no_capacity", err)
	}

	if calls.Load() != 0 {
		t.Errorf("the model must not be called in either shortcut; it was called %d time(s)", calls.Load())
	}
}
