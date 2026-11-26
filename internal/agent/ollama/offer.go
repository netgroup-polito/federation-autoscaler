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
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The provider's side of the AI (phase 1): a provider writes its offer in
// plain words ("0.02 euro per core per hour, green energy, lend half of our
// memory") and the model turns it into the provider's settings, the same ones
// its console sets by hand. Code checks every value against the text, as on
// the consumer side: a small model may invent a number, never a setting the
// text does not state.

// OfferSettings is what a provider's plain-words offer sets. Nil: not stated.
type OfferSettings struct {
	// Prices are in EUR per hour: per CPU core, per GiB of memory, per GPU.
	CPUPrice    *float64 `json:"cpuPrice,omitempty"`
	MemoryPrice *float64 `json:"memoryPrice,omitempty"`
	GPUPrice    *float64 `json:"gpuPrice,omitempty"`
	// Renewable is whether the energy is renewable.
	Renewable *bool `json:"renewable,omitempty"`
	// CPUSharePercent and MemorySharePercent are how much of the cluster to
	// lend, 1 to 100.
	CPUSharePercent    *int `json:"cpuSharePercent,omitempty"`
	MemorySharePercent *int `json:"memorySharePercent,omitempty"`
}

// Empty reports whether nothing is set.
func (s OfferSettings) Empty() bool {
	return s.CPUPrice == nil && s.MemoryPrice == nil && s.GPUPrice == nil &&
		s.Renewable == nil && s.CPUSharePercent == nil && s.MemorySharePercent == nil
}

// String is a short reading, e.g. "CPU 0.02 EUR/h, memory share 50%".
func (s OfferSettings) String() string {
	var parts []string
	price := func(name string, v *float64, unit string) {
		if v != nil {
			parts = append(parts, fmt.Sprintf("%s %s EUR/%s/h", name, strconv.FormatFloat(*v, 'f', -1, 64), unit))
		}
	}
	price("CPU", s.CPUPrice, "core")
	price("memory", s.MemoryPrice, "GiB")
	price("GPU", s.GPUPrice, "GPU")
	if s.Renewable != nil {
		parts = append(parts, fmt.Sprintf("renewable %t", *s.Renewable))
	}
	if s.CPUSharePercent != nil {
		parts = append(parts, fmt.Sprintf("CPU share %d%%", *s.CPUSharePercent))
	}
	if s.MemorySharePercent != nil {
		parts = append(parts, fmt.Sprintf("memory share %d%%", *s.MemorySharePercent))
	}
	if len(parts) == 0 {
		return "nothing understood"
	}
	return strings.Join(parts, ", ")
}

// OfferSystemPrompt asks the model to read a provider's offer into settings.
const OfferSystemPrompt = `You read a cloud provider's offer, written in plain words, and return its settings as JSON.

Return exactly this JSON object, with null for anything the offer does not state:
{"cpu_price": number|null, "memory_price": number|null, "gpu_price": number|null,
 "renewable": true|false|null, "cpu_share_percent": integer|null, "memory_share_percent": integer|null}

Rules:
- Prices are in euro per hour: cpu_price per CPU core, memory_price per GB of memory, gpu_price per GPU.
- renewable is true when the energy is said to be renewable or green (solar, wind, hydro), false when it is said not to be.
- cpu_share_percent and memory_share_percent are how much of the cluster's CPU or memory to lend, from 1 to 100 ("half" is 50, "all" is 100).
- Use only numbers written in the offer. Never guess a value.`

// offerResponse is the model's JSON answer.
type offerResponse struct {
	CPUPrice           *float64 `json:"cpu_price"`
	MemoryPrice        *float64 `json:"memory_price"`
	GPUPrice           *float64 `json:"gpu_price"`
	Renewable          *bool    `json:"renewable"`
	CPUSharePercent    *float64 `json:"cpu_share_percent"`
	MemorySharePercent *float64 `json:"memory_share_percent"`
}

// ExtractOffer asks the model for the offer's settings. The returned Trace is
// never nil; on error the caller falls back to ParseOffer.
func (c *Client) ExtractOffer(ctx context.Context, text string) (*Trace, OfferSettings, error) {
	trace := &Trace{
		Model:        c.model,
		API:          c.api,
		SystemPrompt: OfferSystemPrompt,
		UserPrompt:   "Offer: " + text,
		Format:       jsonMode,
		StartedAt:    time.Now(),
	}
	envelope, serr := c.generate(ctx, trace)
	if serr != nil {
		_, err := trace.fail(serr)
		return trace, OfferSettings{}, err
	}
	trace.Envelope = envelope
	trace.RawResponse = envelope.Response

	var r offerResponse
	if err := json.Unmarshal([]byte(envelope.Response), &r); err != nil {
		_, ferr := trace.fail(selectErr(ErrKindInvalidJSON, "parse offer JSON %q: %w", envelope.Response, err))
		return trace, OfferSettings{}, ferr
	}
	trace.FinishedAt = time.Now()
	return trace, checkOffer(r, text), nil
}

// checkOffer keeps only what the text supports: a number must be written in
// it, a share must be 1 to 100, and "renewable" needs a word about energy.
func checkOffer(r offerResponse, text string) OfferSettings {
	var s OfferSettings
	nums := numbersIn(text)
	keepPrice := func(v *float64) *float64 {
		if v == nil || *v < 0 || !hasNumber(nums, *v) {
			return nil
		}
		x := *v
		return &x
	}
	keepShare := func(v *float64) *int {
		if v == nil || *v < 1 || *v > 100 || *v != math.Trunc(*v) {
			return nil
		}
		n := int(*v)
		written := hasNumber(nums, *v) ||
			(n == 50 && halfRe.MatchString(text)) ||
			(n == 100 && allRe.MatchString(text))
		if !written {
			return nil
		}
		return &n
	}
	s.CPUPrice = keepPrice(r.CPUPrice)
	s.MemoryPrice = keepPrice(r.MemoryPrice)
	s.GPUPrice = keepPrice(r.GPUPrice)
	s.CPUSharePercent = keepShare(r.CPUSharePercent)
	s.MemorySharePercent = keepShare(r.MemorySharePercent)
	if r.Renewable != nil {
		switch {
		case notRenewableRe.MatchString(text):
			f := false
			s.Renewable = &f
		case renewableRe.MatchString(text):
			v := *r.Renewable
			s.Renewable = &v
		}
	}
	return s
}

// Fixed patterns for ParseOffer and the checks.
var (
	numberRe       = regexp.MustCompile(`\d+(?:[.,]\d+)?`)
	priceRe        = `(?:€\s*)?(` + `\d+(?:[.,]\d+)?` + `)\s*(?:€|eur(?:os?)?)?\s*(?:/|per|an?|each)\s*`
	cpuPriceRe     = regexp.MustCompile(`(?i)` + priceRe + `(?:cpus?|cores?|vcpus?)\b`)
	memPriceRe     = regexp.MustCompile(`(?i)` + priceRe + `(?:gb|gib|gigabytes?)\b(?:\s+of\s+(?:memory|ram))?`)
	gpuPriceRe     = regexp.MustCompile(`(?i)` + priceRe + `gpus?\b`)
	cpuShareRe     = regexp.MustCompile(`(?i)(\d{1,3})\s*%\s*of\s+(?:our\s+|the\s+|its\s+)?(?:cpus?|cores?|compute)\b`)
	memShareRe     = regexp.MustCompile(`(?i)(\d{1,3})\s*%\s*of\s+(?:our\s+|the\s+|its\s+)?(?:memory|ram)\b`)
	cpuHalfRe      = regexp.MustCompile(`(?i)\bhalf\s+(?:of\s+)?(?:our\s+|the\s+|its\s+)?(?:cpus?|cores?|compute)\b`)
	memHalfRe      = regexp.MustCompile(`(?i)\bhalf\s+(?:of\s+)?(?:our\s+|the\s+|its\s+)?(?:memory|ram)\b`)
	halfRe         = regexp.MustCompile(`(?i)\bhalf\b`)
	allRe          = regexp.MustCompile(`(?i)\ball\b`)
	notRenewableRe = regexp.MustCompile(`(?i)\b(?:not|no|non)[- ]?(?:renewable|green)\b`)
)

// numbersIn returns every number written in the text.
func numbersIn(text string) []float64 {
	var out []float64
	for _, m := range numberRe.FindAllString(text, -1) {
		if v, err := strconv.ParseFloat(strings.ReplaceAll(m, ",", "."), 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// hasNumber reports whether v is one of the numbers in the text.
func hasNumber(nums []float64, v float64) bool {
	for _, n := range nums {
		if math.Abs(n-v) < 1e-9 {
			return true
		}
	}
	return false
}

// ParseOffer reads an offer with fixed patterns. It is the fallback when the
// model is not available.
func ParseOffer(text string) OfferSettings {
	var s OfferSettings
	price := func(re *regexp.Regexp) *float64 {
		m := re.FindStringSubmatch(text)
		if m == nil {
			return nil
		}
		v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64)
		if err != nil {
			return nil
		}
		return &v
	}
	share := func(re, half *regexp.Regexp) *int {
		if m := re.FindStringSubmatch(text); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n >= 1 && n <= 100 {
				return &n
			}
		}
		if half.MatchString(text) {
			n := 50
			return &n
		}
		return nil
	}
	s.CPUPrice = price(cpuPriceRe)
	s.MemoryPrice = price(memPriceRe)
	s.GPUPrice = price(gpuPriceRe)
	s.CPUSharePercent = share(cpuShareRe, cpuHalfRe)
	s.MemorySharePercent = share(memShareRe, memHalfRe)
	switch {
	case notRenewableRe.MatchString(text):
		f := false
		s.Renewable = &f
	case renewableRe.MatchString(text):
		t := true
		s.Renewable = &t
	}
	return s
}

// OfferResult is how an offer was read.
type OfferResult struct {
	Settings OfferSettings
	// Source is AmountSourceAI (the model) or AmountSourceText (fixed patterns).
	Source string
	// Trace is the model call, nil when the model was not called.
	Trace *Trace
	// AIError is why the model's answer was not used, empty otherwise.
	AIError string
}

// UnderstandOffer reads a provider's offer: from the model when c is not nil
// and its checked answer sets something, else with ParseOffer.
func UnderstandOffer(ctx context.Context, c *Client, text string) OfferResult {
	var res OfferResult
	if c != nil {
		trace, s, err := c.ExtractOffer(ctx, text)
		res.Trace = trace
		switch {
		case err != nil:
			res.AIError = err.Error()
		case s.Empty():
			res.AIError = "the model's reading sets nothing the text supports"
		default:
			res.Settings, res.Source = s, AmountSourceAI
			return res
		}
	}
	res.Settings, res.Source = ParseOffer(text), AmountSourceText
	return res
}
