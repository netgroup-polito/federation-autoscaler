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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// AmountSystemPrompt asks the model for the amount of capacity a plain-text
// request states, and nothing else: no ranking, no guessing. "Not stated" is
// null, so a request like "closest to me" yields an empty amount and the
// caller applies its own default.
const AmountSystemPrompt = `You read a request for computing resources and extract the amount it asks for.

Answer with exactly one JSON object and nothing else:
{"cpu": <number of CPU cores, or null>, "memoryGiB": <GiB of memory, or null>, "gpu": <number of GPUs, or null>, "nodes": <number of nodes, machines or servers, or null>, "maxLatencyMs": <the highest latency allowed, in milliseconds, or null>}

Rules:
- Use null for anything the request does not state with a number (or a number word such as "two", or "a GPU" meaning 1).
- "cores", "vCPU", "vCPUs", "processors" and "CPUs" are CPU cores.
- "nodes", "machines", "servers", "VMs" and "instances" are nodes.
- maxLatencyMs is a limit the request writes as a number of milliseconds, such as "at most 7 ms", "max 12ms", "within 20 milliseconds". Copy that number.
- If the request has no number followed by "ms" or "milliseconds", maxLatencyMs MUST be null. "lowest latency", "low latency", "fast" and "as fast as possible" are wishes, not limits: null. Never reuse the number of nodes, CPUs or GB as a latency.
- Memory: GB, GiB, "gigs" and "G of RAM" are GiB; MB and MiB are divided by 1024; TB and TiB are multiplied by 1024.
- Words like "small", "big", "a bit", "enough" are not amounts: use null.
- Never invent an amount that is not in the request.`

// Bounds a stated amount must respect to be taken at face value. Anything
// outside them is treated as a misreading, not as a request.
const (
	maxAmountCPU       = 256.0  // cores
	maxAmountMemoryGiB = 2048.0 // GiB
	maxAmountGPU       = 16
	maxAmountNodes     = 16
	maxAmountLatencyMs = 10000
)

// Sources of an understood Amount.
const (
	// AmountSourceAI: the model read the amount from the text.
	AmountSourceAI = "ai"
	// AmountSourceText: the model was not used or gave no valid answer, and the
	// amount was read from the text with fixed patterns.
	AmountSourceText = "text"
	// AmountSourceNone: the text states no amount; the caller applies its
	// default.
	AmountSourceNone = "none"
)

// Amount is the capacity a request asks for. A zero field means "not stated".
// Nodes is a count of nodes ("3 nodes"); the caller sizes each one.
// MaxLatencyMs is a latency limit the request sets ("at most 3 ms"), not an
// amount of capacity.
type Amount struct {
	CPU          resource.Quantity
	Memory       resource.Quantity
	GPU          int64
	Nodes        int64
	MaxLatencyMs int32
}

// HasSize reports whether the request states any capacity (resources or a
// node count). A request with only a latency limit has no size.
func (a Amount) HasSize() bool {
	return !a.CPU.IsZero() || !a.Memory.IsZero() || a.GPU > 0 || a.Nodes > 0
}

// IsZero reports whether the request states nothing: no size and no latency
// limit.
func (a Amount) IsZero() bool {
	return !a.HasSize() && a.MaxLatencyMs == 0
}

// ResourceList returns the stated resources, leaving out the ones not stated.
func (a Amount) ResourceList() corev1.ResourceList {
	out := corev1.ResourceList{}
	if !a.CPU.IsZero() {
		out[corev1.ResourceCPU] = a.CPU.DeepCopy()
	}
	if !a.Memory.IsZero() {
		out[corev1.ResourceMemory] = a.Memory.DeepCopy()
	}
	if a.GPU > 0 {
		out[corev1.ResourceName("nvidia.com/gpu")] = *resource.NewQuantity(a.GPU, resource.DecimalSI)
	}
	return out
}

// String renders the amount for logs and the console, e.g. "4 CPU, 8Gi memory".
func (a Amount) String() string {
	if a.IsZero() {
		return "not stated"
	}
	var parts []string
	if !a.CPU.IsZero() {
		parts = append(parts, a.CPU.String()+" CPU")
	}
	if !a.Memory.IsZero() {
		parts = append(parts, a.Memory.String()+" memory")
	}
	if a.GPU > 0 {
		parts = append(parts, fmt.Sprintf("%d GPU", a.GPU))
	}
	if a.Nodes == 1 {
		parts = append(parts, "1 node")
	} else if a.Nodes > 1 {
		parts = append(parts, fmt.Sprintf("%d nodes", a.Nodes))
	}
	if a.MaxLatencyMs > 0 {
		parts = append(parts, fmt.Sprintf("max %d ms", a.MaxLatencyMs))
	}
	return strings.Join(parts, ", ")
}

// amountResponse is the JSON object the model is asked for.
type amountResponse struct {
	CPU       *float64 `json:"cpu"`
	MemoryGiB *float64 `json:"memoryGiB"`
	GPU       *float64 `json:"gpu"`
	Nodes     *float64 `json:"nodes"`
	MaxLatMs  *float64 `json:"maxLatencyMs"`
}

// toAmount validates the model's answer and converts it. An answer with any
// value out of bounds (negative, absurdly large, a fractional GPU) is rejected
// as a whole: a misread number must not become a booking.
func (r amountResponse) toAmount() (Amount, error) {
	var a Amount
	if r.CPU != nil {
		if *r.CPU < 0 || *r.CPU > maxAmountCPU || math.IsNaN(*r.CPU) {
			return Amount{}, fmt.Errorf("cpu %v out of range [0, %v]", *r.CPU, maxAmountCPU)
		}
		a.CPU = *resource.NewMilliQuantity(int64(math.Round(*r.CPU*1000)), resource.DecimalSI)
	}
	if r.MemoryGiB != nil {
		if *r.MemoryGiB < 0 || *r.MemoryGiB > maxAmountMemoryGiB || math.IsNaN(*r.MemoryGiB) {
			return Amount{}, fmt.Errorf("memoryGiB %v out of range [0, %v]", *r.MemoryGiB, maxAmountMemoryGiB)
		}
		a.Memory = memoryFromGiB(*r.MemoryGiB)
	}
	if r.GPU != nil {
		g := *r.GPU
		if g < 0 || g > maxAmountGPU || g != math.Trunc(g) {
			return Amount{}, fmt.Errorf("gpu %v is not a whole number in [0, %d]", g, maxAmountGPU)
		}
		a.GPU = int64(g)
	}
	if r.Nodes != nil {
		n := *r.Nodes
		if n < 0 || n > maxAmountNodes || n != math.Trunc(n) {
			return Amount{}, fmt.Errorf("nodes %v is not a whole number in [0, %d]", n, maxAmountNodes)
		}
		a.Nodes = int64(n)
	}
	if r.MaxLatMs != nil {
		ms := math.Ceil(*r.MaxLatMs) // a limit rounds up, never tighter than asked
		if math.IsNaN(ms) || ms < 1 || ms > maxAmountLatencyMs {
			return Amount{}, fmt.Errorf("maxLatencyMs %v out of range [1, %d]", *r.MaxLatMs, maxAmountLatencyMs)
		}
		a.MaxLatencyMs = int32(ms)
	}
	return a, nil
}

// memoryFromGiB converts GiB to a binary quantity, rounded to whole MiB.
func memoryFromGiB(gib float64) resource.Quantity {
	mib := int64(math.Round(gib * 1024))
	if mib%1024 == 0 {
		return *resource.NewQuantity(mib/1024*1024*1024*1024, resource.BinarySI)
	}
	return *resource.NewQuantity(mib*1024*1024, resource.BinarySI)
}

// ExtractAmount asks the model for the amount the text states. The returned
// Trace is never nil; on error the caller falls back to ParseAmount.
func (c *Client) ExtractAmount(ctx context.Context, text string) (*Trace, Amount, error) {
	trace := &Trace{
		Model:        c.model,
		SystemPrompt: AmountSystemPrompt,
		UserPrompt:   "Request: " + text,
		Format:       jsonMode,
		StartedAt:    time.Now(),
	}
	envelope, serr := c.generate(ctx, trace)
	if serr != nil {
		_, err := trace.fail(serr)
		return trace, Amount{}, err
	}
	trace.Envelope = envelope
	trace.RawResponse = envelope.Response

	var parsed amountResponse
	if err := json.Unmarshal([]byte(envelope.Response), &parsed); err != nil {
		_, ferr := trace.fail(selectErr(ErrKindInvalidJSON, "parse LLM amount JSON %q: %w", envelope.Response, err))
		return trace, Amount{}, ferr
	}
	amount, err := parsed.toAmount()
	if err != nil {
		_, ferr := trace.fail(selectErr(ErrKindInvalidJSON, "LLM amount %q: %w", envelope.Response, err))
		return trace, Amount{}, ferr
	}
	trace.FinishedAt = time.Now()
	return trace, amount, nil
}

// AmountResult is what UnderstandAmount concluded, and how.
type AmountResult struct {
	Amount Amount
	// Source is AmountSourceAI, AmountSourceText or AmountSourceNone.
	Source string
	// Trace is the model call, nil when the model was not called.
	Trace *Trace
	// AIError is why the model's answer was not used, empty when it was used
	// or not called.
	AIError string
}

// UnderstandAmount reads the amount a plain-text request states. It asks the
// model when c is not nil and uses its answer when valid; otherwise it reads
// the text with ParseAmount. A request that states no amount gives Source
// AmountSourceNone and a zero Amount, and the caller applies its default.
func UnderstandAmount(ctx context.Context, c *Client, text string) AmountResult {
	var res AmountResult
	if c != nil {
		trace, amount, err := c.ExtractAmount(ctx, text)
		res.Trace = trace
		if err == nil {
			// A latency limit must be written in the text as "<number> ms": a
			// model can borrow another number from the request ("3 VMs ...
			// lowest latency") and a made-up limit would exclude providers.
			if !latencyNumberRe.MatchString(text) {
				amount.MaxLatencyMs = 0
			}
			res.Amount = amount
			res.Source = AmountSourceAI
			if amount.IsZero() {
				res.Source = AmountSourceNone
			}
			return res
		}
		res.AIError = err.Error()
	}
	res.Amount = ParseAmount(text)
	res.Source = AmountSourceText
	if res.Amount.IsZero() {
		res.Source = AmountSourceNone
	}
	return res
}

// Patterns for ParseAmount. Numbers are digits ("4", "2.5", "2,5") or the
// number words in numberWords; "a"/"an" counts as 1 only before "GPU".
var (
	numPattern      = `(\d+(?:[.,]\d+)?|one|two|three|four|five|six|seven|eight|nine|ten|twelve|sixteen)`
	cpuRe           = regexp.MustCompile(`(?i)\b` + numPattern + `\s*(?:x\s*)?(?:v?cpus?|v?cores?|processors?)\b`)
	memRe           = regexp.MustCompile(`(?i)\b` + numPattern + `\s*(tib|tb|gib|gb|gigs?|mib|mb)\b|\b` + numPattern + `\s*(g|m)\s+(?:of\s+)?(?:ram|memory)\b`)
	nodesRe         = regexp.MustCompile(`(?i)\b` + numPattern + `\s*(?:x\s*)?(?:nodes?|machines?|servers?|vms?|instances?)\b`)
	latencyNumberRe = regexp.MustCompile(`(?i)\b` + numPattern + `\s*(?:ms|msec|milliseconds?)\b`)
	latencyRe       = regexp.MustCompile(`(?i)\b(?:at\s+most|max(?:imum)?|within|under|below|less\s+than|no\s+more\s+than|up\s+to)\s*(\d+(?:[.,]\d+)?)\s*(?:ms|msec|milliseconds?)\b`)
	gpuRe           = regexp.MustCompile(`(?i)\b(?:` + numPattern + `|an?)\s*(?:x\s*)?(?:nvidia\s+)?gpus?\b`)

	numberWords = map[string]float64{
		"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7,
		"eight": 8, "nine": 9, "ten": 10, "twelve": 12, "sixteen": 16, "a": 1, "an": 1,
	}
)

func parseNumber(s string) (float64, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if v, ok := numberWords[s]; ok {
		return v, true
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
	return v, err == nil
}

// ParseAmount reads the amount a text states with fixed patterns: "4 CPUs",
// "2 cores", "8 GB", "512 MB", "16G of RAM", "a GPU", "two GPUs", "3 nodes". It is the
// fallback when the model is not available, and never guesses: a text with no
// stated amount gives a zero Amount. Out-of-range values are ignored.
func ParseAmount(text string) Amount {
	var r amountResponse
	if m := cpuRe.FindStringSubmatch(text); m != nil {
		if v, ok := parseNumber(m[1]); ok {
			r.CPU = &v
		}
	}
	if m := memRe.FindStringSubmatch(text); m != nil {
		num, unit := m[1], strings.ToLower(m[2])
		if num == "" {
			num, unit = m[3], strings.ToLower(m[4])
		}
		if v, ok := parseNumber(num); ok {
			switch unit {
			case "tib", "tb":
				v *= 1024
			case "mib", "mb", "m":
				v /= 1024
			}
			r.MemoryGiB = &v
		}
	}
	if m := gpuRe.FindStringSubmatch(text); m != nil {
		word := m[1]
		if word == "" { // "a GPU" / "an GPU"
			word = "a"
		}
		if v, ok := parseNumber(word); ok {
			r.GPU = &v
		}
	}
	if m := nodesRe.FindStringSubmatch(text); m != nil {
		if v, ok := parseNumber(m[1]); ok {
			r.Nodes = &v
		}
	}
	if m := latencyRe.FindStringSubmatch(text); m != nil {
		if v, ok := parseNumber(m[1]); ok {
			r.MaxLatMs = &v
		}
	}
	// Drop any value that is out of range instead of rejecting the rest.
	var a Amount
	for _, one := range []amountResponse{{CPU: r.CPU}, {MemoryGiB: r.MemoryGiB}, {GPU: r.GPU}, {Nodes: r.Nodes}, {MaxLatMs: r.MaxLatMs}} {
		if part, err := one.toAmount(); err == nil {
			if !part.CPU.IsZero() {
				a.CPU = part.CPU
			}
			if !part.Memory.IsZero() {
				a.Memory = part.Memory
			}
			if part.GPU > 0 {
				a.GPU = part.GPU
			}
			if part.Nodes > 0 {
				a.Nodes = part.Nodes
			}
			if part.MaxLatencyMs > 0 {
				a.MaxLatencyMs = part.MaxLatencyMs
			}
		}
	}
	return a
}
