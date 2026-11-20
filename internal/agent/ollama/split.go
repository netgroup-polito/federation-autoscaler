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
	"fmt"
	"regexp"
	"strings"
	"time"
)

// MaxSplitParts is the most parts one request is split into.
const MaxSplitParts = 4

// SplitSystemPrompt asks the model to split a request that asks for resources
// in several places ("2 VMs in Turin and 1 VM in Frankfurt") into one
// request per place, each complete on its own.
const SplitSystemPrompt = `You read a request for computing resources. If it asks for resources in two or more different places, split it into one request per place. Otherwise keep it as one request.

Answer with exactly one JSON object and nothing else:
{"parts": [<one string per request>]}

Rules:
- Split only by place: "2 VMs in Turin and 1 VM in Frankfurt" gives ["2 VMs in Turin", "1 VM in Frankfurt"].
- Each part must be complete: repeat the unit and every condition that applies to all parts. "2 VMs in Turin and 1 in Frankfurt, at most 10 ms" gives ["2 VMs in Turin, at most 10 ms", "1 VM in Frankfurt, at most 10 ms"].
- Do not split amounts of one request: "4 CPUs and 8 GB in Turin" stays ["4 CPUs and 8 GB in Turin"].
- Do not split choices between places: "a VM in Turin or Milan" stays one request.
- Never invent a place, an amount or a condition that is not in the request.`

// splitHintRe finds place markers ("in Turin", "at Frankfurt"): a request is
// asked to the model for splitting only when it has two or more of them and
// a conjunction, so an ordinary request costs no extra call.
var (
	splitHintRe        = regexp.MustCompile(`\b(?:in|at|from|near)\s+(\p{Lu}[\p{L}'-]*)`)
	splitConjunctionRe = regexp.MustCompile(`(?i)\band\b|\bplus\b|[,;]`)
)

// orChoiceRe finds a choice between places ("in Turin or in Cuneo"), which is
// one request with a choice, never two requests.
var orChoiceRe = regexp.MustCompile(`\b(?:in|at|from|near)\s+\p{Lu}[\p{L}'-]*\s*,?\s+or\s+(?:(?:in|at|from|near)\s+)?\p{Lu}`)

// emptyPartRe finds a part that asks for nothing ("nothing in Frankfurt",
// "0 VMs in Frankfurt"): that is a condition of the request, not a request.
var emptyPartRe = regexp.MustCompile(`(?i)^\s*(?:nothing|none|no|not|except|without|zero|0)\b`)

// MightSplit reports whether text may ask for resources in several places.
func MightSplit(text string) bool {
	return len(splitHintRe.FindAllStringIndex(text, -1)) >= 2 && splitConjunctionRe.MatchString(text) &&
		!orChoiceRe.MatchString(text)
}

// SplitResult is what UnderstandSplit concluded.
type SplitResult struct {
	// Parts are the requests, one per place; a single part means no split.
	Parts []string
	// Trace is the model call, nil when the model was not called.
	Trace *Trace
	// AIError is why the model's answer was not used, empty otherwise.
	AIError string
}

// UnderstandSplit splits a request asking for several places into one
// request per place. Without a model, or when the text does not look like a
// multi-place request, or when the answer is not usable, the request stays
// whole: one part, the text itself.
func UnderstandSplit(ctx context.Context, c *Client, text string) SplitResult {
	whole := SplitResult{Parts: []string{text}}
	if c == nil || !MightSplit(text) {
		return whole
	}
	trace, parts, err := c.ExtractSplit(ctx, text)
	if err != nil {
		whole.Trace, whole.AIError = trace, err.Error()
		return whole
	}
	return SplitResult{Parts: parts, Trace: trace}
}

// ExtractSplit asks the model to split text. The returned Trace is never nil.
func (c *Client) ExtractSplit(ctx context.Context, text string) (*Trace, []string, error) {
	trace := &Trace{
		Model:        c.model,
		API:          c.api,
		SystemPrompt: SplitSystemPrompt,
		UserPrompt:   "Request: " + text,
		Format:       jsonMode,
		StartedAt:    time.Now(),
	}
	envelope, serr := c.generate(ctx, trace)
	if serr != nil {
		_, err := trace.fail(serr)
		return trace, nil, err
	}
	trace.Envelope = envelope
	trace.RawResponse = envelope.Response

	var r struct {
		Parts []string `json:"parts"`
	}
	if err := json.Unmarshal([]byte(envelope.Response), &r); err != nil {
		_, ferr := trace.fail(selectErr(ErrKindInvalidJSON, "parse split JSON %q: %w", envelope.Response, err))
		return trace, nil, ferr
	}
	parts, err := checkParts(r.Parts, text)
	if err != nil {
		_, ferr := trace.fail(selectErr(ErrKindInvalidJSON, "invalid split %q: %w", envelope.Response, err))
		return trace, nil, ferr
	}
	trace.FinishedAt = time.Now()
	return trace, parts, nil
}

// checkParts validates a split: 1 to MaxSplitParts non-empty, distinct parts,
// none longer than the request, and every place a part names must be in the
// request (a model can invent one). A single part is returned as the whole
// text, so "no split" never changes the request.
func checkParts(parts []string, text string) ([]string, error) {
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		key := strings.ToLower(p)
		if p == "" || seen[key] || emptyPartRe.MatchString(p) {
			continue
		}
		if len(p) > len(text)+64 {
			return nil, fmt.Errorf("part %q is longer than the request", p)
		}
		for _, m := range splitHintRe.FindAllStringSubmatch(p, -1) {
			if place := m[1]; !placeRe(place).MatchString(text) {
				return nil, fmt.Errorf("part %q names %q, which is not in the request", p, place)
			}
		}
		seen[key] = true
		out = append(out, p)
	}
	switch {
	case len(out) == 0:
		return nil, fmt.Errorf("no parts")
	case len(out) > MaxSplitParts:
		return nil, fmt.Errorf("%d parts; at most %d", len(out), MaxSplitParts)
	case len(out) == 1:
		return []string{text}, nil
	}
	return out, nil
}
