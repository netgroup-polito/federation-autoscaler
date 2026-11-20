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
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestMightSplit(t *testing.T) {
	for text, want := range map[string]bool{
		"I need 2 VMs in Turin and 1 VM in Frankfurt":      true,
		"2 VMs in Turin, 1 in Frankfurt":                   true,
		"4 CPUs and 8 GB in Turin":                         false, // one place
		"I need 2 VMs in Turin":                            false,
		"the cheapest and the greenest":                    false, // no place
		"a VM in Turin or in Milan":                        false, // no conjunction: a choice
		"A VM in Turin or in Cuneo, whichever is cheaper.": false, // a choice, despite the comma
	} {
		if got := MightSplit(text); got != want {
			t.Errorf("MightSplit(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestCheckParts(t *testing.T) {
	text := "I need 2 VMs in Turin and 1 VM in Frankfurt"
	if got, err := checkParts([]string{" 2 VMs in Turin ", "1 VM in Frankfurt", "1 VM in Frankfurt"}, text); err != nil ||
		!reflect.DeepEqual(got, []string{"2 VMs in Turin", "1 VM in Frankfurt"}) {
		t.Errorf("got %v, %v", got, err)
	}
	if got, err := checkParts([]string{"2 VMs in Turin and 1 VM in Frankfurt"}, text); err != nil || !reflect.DeepEqual(got, []string{text}) {
		t.Errorf("one part must be the whole text: %v, %v", got, err)
	}
	// A part that asks for nothing is a condition, not a request: the
	// request stays whole.
	cond := "I need 3 VMs in Turin, the cheapest, and nothing in Frankfurt."
	if got, err := checkParts([]string{"3 VMs in Turin, the cheapest", "nothing in Frankfurt"}, cond); err != nil ||
		!reflect.DeepEqual(got, []string{cond}) {
		t.Errorf("a part asking for nothing must leave the request whole: %v, %v", got, err)
	}
	if _, err := checkParts([]string{"2 VMs in Turin", "1 VM in Milan"}, text); err == nil {
		t.Error("a place not in the request must be rejected")
	}
	if _, err := checkParts([]string{"a in Turin", "b in Turin", "c in Turin", "d in Turin", "e in Turin"}, text); err == nil {
		t.Error("more than MaxSplitParts parts must be rejected")
	}
}

func TestUnderstandSplit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollamaRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.System != SplitSystemPrompt {
			t.Errorf("system prompt = %q", req.System)
		}
		_ = json.NewEncoder(w).Encode(ollamaResponse{Response: `{"parts":["2 VMs in Turin, at most 10 ms","1 VM in Frankfurt, at most 10 ms"]}`, Done: true})
	}))
	defer srv.Close()
	c := New(srv.URL, "m")

	res := UnderstandSplit(context.Background(), c, "2 VMs in Turin and 1 in Frankfurt, at most 10 ms")
	if len(res.Parts) != 2 || res.AIError != "" {
		t.Errorf("result %+v", res)
	}
	// An ordinary request is not sent to the model at all.
	if res := UnderstandSplit(context.Background(), c, "I need 2 VMs in Turin"); res.Trace != nil || len(res.Parts) != 1 {
		t.Errorf("ordinary request: %+v", res)
	}
	// Without a model the request stays whole.
	if res := UnderstandSplit(context.Background(), nil, "2 VMs in Turin and 1 in Frankfurt"); len(res.Parts) != 1 {
		t.Errorf("no model: %+v", res)
	}
}
