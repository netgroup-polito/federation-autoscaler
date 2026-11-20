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
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// The jobs the AI can do for a plain-language request (the agent's --ai-job).
//
// JobRank is the original job: the model receives the request and every
// provider's data and ranks the providers itself.
//
// JobFields is the lighter job: the model only turns the request into a few
// fields (Intent), and RankByIntent filters and ranks the providers in code.
// The question is one short sentence, so a much smaller model is enough, and
// the ranking is exact and the same every time.
const (
	JobRank   = "rank"
	JobFields = "fields"
)

// ValidJob reports whether job is one the agent knows ("" means JobRank).
func ValidJob(job string) bool {
	return job == "" || job == JobRank || job == JobFields
}

// The preferences an Intent can carry, one at most.
const (
	PreferNone          = ""
	PreferLatency       = "latency"       // lowest measured round-trip time
	PreferNearest       = "nearest"       // shortest distance to the consumer
	PreferPrice         = "price"         // lowest price per node
	PreferPricePerCPU   = "pricePerCpu"   // lowest price per CPU core
	PreferPricePerGiB   = "pricePerGiB"   // lowest price per GiB of memory
	PreferGreen         = "green"         // lowest carbon intensity now
	PreferGreenForecast = "greenForecast" // lowest carbon over the next hours
	PreferCapacity      = "capacity"      // most free capacity
	maxIntentPlaces     = 8               // a longer list is a misreading
	maxIntentHours      = 24 * 30         // the catalog's longest guarantee
	forecastHoursIntent = 6               // like the rank job's carbonForecast6hAvg
)

var validPreferences = map[string]bool{
	PreferNone: true, PreferLatency: true, PreferNearest: true, PreferPrice: true,
	PreferPricePerCPU: true, PreferPricePerGiB: true, PreferGreen: true,
	PreferGreenForecast: true, PreferCapacity: true,
}

// Intent is what a request asks for besides its amount (read separately, see
// UnderstandAmount) and its latency limit (applied as a latencyBound before
// any ranking). Places and Exclude are city or region names; a provider
// matches one when its city or region is that name (case-insensitive).
type Intent struct {
	// Places: only providers in one of these places are kept ("in Turin").
	Places []string `json:"places,omitempty"`
	// Exclude: providers in these places are dropped ("not in Frankfurt").
	Exclude []string `json:"exclude,omitempty"`
	// RenewableOnly keeps only providers that declare renewable energy.
	RenewableOnly bool `json:"renewableOnly,omitempty"`
	// Prefer is how the remaining providers are ordered (Prefer* constants).
	Prefer string `json:"prefer,omitempty"`
	// MinGuaranteedHours is how long the request needs the resources ("for 3
	// hours"); providers whose guarantee covers it come first.
	MinGuaranteedHours float64 `json:"minGuaranteedHours,omitempty"`
}

// String renders the intent for logs and the dashboard.
func (in Intent) String() string {
	var parts []string
	if len(in.Places) > 0 {
		parts = append(parts, "in "+strings.Join(in.Places, " or "))
	}
	if len(in.Exclude) > 0 {
		parts = append(parts, "not in "+strings.Join(in.Exclude, ", "))
	}
	if in.RenewableOnly {
		parts = append(parts, "renewable only")
	}
	if in.MinGuaranteedHours > 0 {
		parts = append(parts, fmt.Sprintf("for %g h", in.MinGuaranteedHours))
	}
	if in.Prefer != PreferNone {
		parts = append(parts, "prefer "+in.Prefer)
	}
	if len(parts) == 0 {
		return "no preference"
	}
	return strings.Join(parts, ", ")
}

// IntentSystemPrompt asks the model for the request's preferences as fields,
// and nothing else: it sees no provider and ranks nothing.
const IntentSystemPrompt = `You read a request for computing resources and extract what it prefers. You do not see any provider and you do not choose one.

Answer with exactly one JSON object and nothing else:
{"places": [<places the resources must be in>], "exclude": [<places to avoid>], "renewableOnly": <true or false>, "prefer": <one of "latency", "nearest", "price", "pricePerCpu", "pricePerGiB", "green", "greenForecast", "capacity", or null>, "minGuaranteedHours": <hours the resources are needed for, or null>}

Rules:
- places: cities or regions the request says the resources must be in ("in Turin", "at Frankfurt", "in Piedmont"). Copy the names as written. Empty list if none.
- exclude: cities, regions or countries to avoid ("not in X", "outside X", "except X", "no X"). Copy the names as written. Empty list if none.
- renewableOnly: true only for renewable, renewables, solar, wind, hydro or "green energy only".
- prefer: what the request wants most, at most one:
  - "latency": low latency, lowest latency, fast, responsive, round-trip;
  - "nearest": close, closest, near, nearby, proximity;
  - "price": cheap, cheapest, low cost, budget;
  - "pricePerCpu": cheapest per CPU, per core, per vCPU; "pricePerGiB": cheapest per GB of memory;
  - "green": green, greenest, eco, clean, low carbon, sustainable;
  - "greenForecast": greenest over the next hours, later today, tonight, forecast;
  - "capacity": most resources, most capacity;
  - null when the request states no preference.
- A latency limit written in milliseconds ("at most 3 ms") is read elsewhere; ignore it here.
- minGuaranteedHours: how long the resources are needed ("for 3 hours" is 3, "for two days" is 48). null if not stated.
- Never invent a place or a preference that is not in the request.`

// intentResponse is the JSON object the model is asked for.
type intentResponse struct {
	Places        []string `json:"places"`
	Exclude       []string `json:"exclude"`
	RenewableOnly *bool    `json:"renewableOnly"`
	Prefer        *string  `json:"prefer"`
	MinHours      *float64 `json:"minGuaranteedHours"`
}

// toIntent validates the model's answer against the request text. A place the
// text does not contain is dropped (a model can name a city it was not told
// about); an unknown preference or an out-of-range duration rejects the whole
// answer, which then falls back to ParseIntent.
func (r intentResponse) toIntent(text string) (Intent, error) {
	var in Intent
	if len(r.Places) > maxIntentPlaces || len(r.Exclude) > maxIntentPlaces {
		return Intent{}, fmt.Errorf("too many places (%d, %d)", len(r.Places), len(r.Exclude))
	}
	// A "place" the model wrote as "outside X" or "not in X" is an exclusion.
	var places, exclude []string
	exclude = append(exclude, r.Exclude...)
	for _, p := range r.Places {
		if excludePrefixRe.MatchString(strings.TrimSpace(p)) {
			exclude = append(exclude, p)
		} else {
			places = append(places, p)
		}
	}
	in.Exclude = placesInText(exclude, text)
	// A place cannot be both wanted and avoided: avoiding wins.
	for _, p := range placesInText(places, text) {
		if !containsFold(in.Exclude, p) {
			in.Places = append(in.Places, p)
		}
	}
	// Like a latency limit, "renewable only" must be in the text: a model
	// tends to set it for any "green" request, which would drop every
	// provider that is clean but not declared renewable.
	if r.RenewableOnly != nil {
		in.RenewableOnly = *r.RenewableOnly && renewableRe.MatchString(text)
	}
	if r.Prefer != nil {
		p := strings.TrimSpace(*r.Prefer)
		if !validPreferences[p] {
			return Intent{}, fmt.Errorf("unknown preference %q", p)
		}
		in.Prefer = p
	}
	if r.MinHours != nil {
		h := *r.MinHours
		if math.IsNaN(h) || h < 0 || h > maxIntentHours {
			return Intent{}, fmt.Errorf("minGuaranteedHours %v out of range [0, %d]", h, maxIntentHours)
		}
		// A duration must be in the text: a model can turn "4 CPUs" or
		// "6 ms" into hours.
		if durationWordRe.MatchString(text) {
			in.MinGuaranteedHours = h
		}
	}
	return in, nil
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// checkWithText corrects the model's intent with what fixed patterns read
// from the text over the providers' own places, which is exact for what it
// recognises: an exclusion in the text always wins, a known place the model
// missed is added, a preference keyword decides the preference, and a
// duration the text states decides the duration.
func checkWithText(in Intent, text string, knownPlaces []string) Intent {
	pat := ParseIntent(text, knownPlaces)
	for _, x := range pat.Exclude {
		if !containsFold(in.Exclude, x) {
			in.Exclude = append(in.Exclude, x)
		}
	}
	kept := in.Places[:0:0]
	for _, p := range in.Places {
		excluded := false
		for _, x := range in.Exclude {
			if strings.EqualFold(p, x) || placeRe(x).MatchString(p) {
				excluded = true
			}
		}
		if !excluded {
			kept = append(kept, p)
		}
	}
	for _, p := range pat.Places {
		if !containsFold(kept, p) && !containsFold(in.Exclude, p) {
			kept = append(kept, p)
		}
	}
	in.Places = nil
	if len(kept) > 0 {
		in.Places = kept
	}
	if pat.Prefer != PreferNone {
		in.Prefer = pat.Prefer
	}
	if pat.MinGuaranteedHours > 0 {
		in.MinGuaranteedHours = pat.MinGuaranteedHours
	}
	return in
}

// placeRe matches a place name as a whole word. A short all-capitals code
// (a region code such as "HE") must match exactly, so it never matches a word
// like "he" or "the"; any other name matches whatever its case.
func placeRe(name string) *regexp.Regexp {
	flags := "(?i)"
	if len(name) <= 3 && name == strings.ToUpper(name) {
		flags = ""
	}
	return regexp.MustCompile(flags + `(?:^|\W)` + regexp.QuoteMeta(name) + `(?:\W|$)`)
}

// placesInText keeps the names the text actually contains (as whole words),
// trimmed and without duplicates.
func placesInText(names []string, text string) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		n = placePrefixRe.ReplaceAllString(strings.TrimSpace(n), "")
		key := strings.ToLower(n)
		if n == "" || seen[key] || !capitalisedInText(n, text) {
			continue
		}
		seen[key] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// placePrefixRe is a preposition a model may copy with the place ("in
// Turin"); excludePrefixRe marks a place the request avoids ("outside X").
var (
	placePrefixRe   = regexp.MustCompile(`(?i)^(?:(?:not|but\s+not|except|outside(?:\s+of)?|without|no|in|at|near|from)\s+)+`)
	excludePrefixRe = regexp.MustCompile(`(?i)^(?:not|but\s+not|except|outside|without|no)\b`)
	durationWordRe  = regexp.MustCompile(`(?i)\b(?:hours?|hrs?|h|days?|weeks?|tomorrow|tonight)\b`)
)

// capitalisedInText reports whether name appears in text as a whole word
// starting with a capital letter: places are proper nouns, so a model's "us",
// "me" or "closest" is not taken for one.
func capitalisedInText(name, text string) bool {
	for _, loc := range placeRe(name).FindAllStringIndex(text, -1) {
		word := strings.TrimLeftFunc(text[loc[0]:loc[1]], func(r rune) bool { return !unicode.IsLetter(r) })
		if r, _ := utf8.DecodeRuneInString(word); unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

// ExtractIntent asks the model for the request's preferences. The returned
// Trace is never nil; on error the caller falls back to ParseIntent.
func (c *Client) ExtractIntent(ctx context.Context, text string) (*Trace, Intent, error) {
	trace := &Trace{
		Model:        c.model,
		API:          c.api,
		SystemPrompt: IntentSystemPrompt,
		UserPrompt:   "Request: " + text,
		Format:       jsonMode,
		StartedAt:    time.Now(),
	}
	envelope, serr := c.generate(ctx, trace)
	if serr != nil {
		_, err := trace.fail(serr)
		return trace, Intent{}, err
	}
	trace.Envelope = envelope
	trace.RawResponse = envelope.Response

	var r intentResponse
	if err := json.Unmarshal([]byte(envelope.Response), &r); err != nil {
		_, ferr := trace.fail(selectErr(ErrKindInvalidJSON, "parse intent JSON %q: %w", envelope.Response, err))
		return trace, Intent{}, ferr
	}
	in, err := r.toIntent(text)
	if err != nil {
		_, ferr := trace.fail(selectErr(ErrKindInvalidJSON, "invalid intent %q: %w", envelope.Response, err))
		return trace, Intent{}, ferr
	}
	trace.FinishedAt = time.Now()
	return trace, in, nil
}

// Keyword patterns for ParseIntent, checked in this order; the first match
// sets the preference.
var (
	excludeBeforeRe = `(?:not\s+in|outside(?:\s+of)?|except(?:\s+in)?|but\s+not\s+in|no)\s+`
	preferPatterns  = []struct {
		re     *regexp.Regexp
		prefer string
	}{
		{regexp.MustCompile(`(?i)\b(?:next|coming)\s+hours\b|\blater\s+today\b|\btonight\b|\bforecast\b`), PreferGreenForecast},
		{regexp.MustCompile(`(?i)\bper\s+(?:cpu|core|vcpu)\b`), PreferPricePerCPU},
		{regexp.MustCompile(`(?i)\bper\s+(?:gb|gib|memory)\b`), PreferPricePerGiB},
		{regexp.MustCompile(`(?i)\bcheap(?:est|er)?\b|\blow\s+cost\b|\bbudget\b`), PreferPrice},
		{regexp.MustCompile(`(?i)\blatency\b|\bfast(?:est)?\b|\bresponsive\b|\bround[- ]trip\b`), PreferLatency},
		{regexp.MustCompile(`(?i)\bclos(?:e|est)\b|\bnear(?:est|by)?\b|\bproximity\b`), PreferNearest},
		{regexp.MustCompile(`(?i)\bgreen(?:est)?\b|\beco\b|\bclean\b|\blow\s+carbon\b|\bsustainable\b`), PreferGreen},
		{regexp.MustCompile(`(?i)\bmost\s+(?:resources|capacity)\b`), PreferCapacity},
	}
	renewableRe = regexp.MustCompile(`(?i)\brenewables?\b|\bsolar\b|\bwind\b|\bhydro\b|\bgreen\s+energy\b`)
	hoursRe     = regexp.MustCompile(`(?i)\bfor\s+` + numPattern + `\s*(hours?|h|days?)\b`)
)

// ParseIntent reads the preferences with fixed patterns. It is the fallback
// when the model is not available. Places are recognised only among
// knownPlaces (the providers' cities and regions), since without the model
// any capitalised word could be a place.
func ParseIntent(text string, knownPlaces []string) Intent {
	var in Intent
	for _, p := range knownPlaces {
		p = strings.TrimSpace(p)
		if p == "" || !placeRe(p).MatchString(text) {
			continue
		}
		ex := regexp.MustCompile(`(?i)\b` + excludeBeforeRe + `(?:\w+\s+)?` + regexp.QuoteMeta(p) + `\b`)
		if ex.MatchString(text) {
			in.Exclude = appendUnique(in.Exclude, p)
		} else {
			in.Places = appendUnique(in.Places, p)
		}
	}
	in.RenewableOnly = renewableRe.MatchString(text)
	for _, pp := range preferPatterns {
		if pp.re.MatchString(text) {
			in.Prefer = pp.prefer
			break
		}
	}
	// "green energy" is renewable, not a carbon preference on its own.
	if in.RenewableOnly && in.Prefer == PreferGreen && !regexp.MustCompile(`(?i)\bgreenest\b`).MatchString(text) {
		in.Prefer = PreferNone
	}
	if m := hoursRe.FindStringSubmatch(text); m != nil {
		if v, ok := parseNumber(m[1]); ok && v > 0 {
			if strings.HasPrefix(strings.ToLower(m[2]), "d") {
				v *= 24
			}
			if v <= maxIntentHours {
				in.MinGuaranteedHours = v
			}
		}
	}
	return in
}

func appendUnique(list []string, s string) []string {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return list
		}
	}
	return append(list, s)
}

// KnownPlaces lists the providers' cities and regions, for ParseIntent.
func KnownPlaces(groups []brokerapi.NodeGroupView) []string {
	var out []string
	for _, ng := range groups {
		if ng.Topology == nil {
			continue
		}
		for _, p := range []string{ng.Topology.City, ng.Topology.Region} {
			if p != "" {
				out = appendUnique(out, p)
			}
		}
	}
	return out
}

// IntentResult is what UnderstandIntent concluded, and how.
type IntentResult struct {
	Intent Intent
	// Source is AmountSourceAI (the model) or AmountSourceText (fixed patterns).
	Source string
	// Trace is the model call, nil when the model was not called.
	Trace *Trace
	// AIError is why the model's answer was not used, empty otherwise.
	AIError string
}

// UnderstandIntent reads the request's preferences: from the model when c is
// not nil and its answer is valid, else with ParseIntent over knownPlaces.
// Places the model names are also kept to known ones when any are known.
func UnderstandIntent(ctx context.Context, c *Client, text string, knownPlaces []string) IntentResult {
	var res IntentResult
	if c != nil {
		trace, in, err := c.ExtractIntent(ctx, text)
		res.Trace = trace
		if err == nil {
			res.Intent = checkWithText(in, text, knownPlaces)
			res.Source = AmountSourceAI
			return res
		}
		res.AIError = err.Error()
	}
	res.Intent = ParseIntent(text, knownPlaces)
	res.Source = AmountSourceText
	return res
}

// RankByIntent filters and orders the providers for an intent, in code:
//  1. drop providers in an excluded place;
//  2. with places, keep only providers in one of them;
//  3. with RenewableOnly, keep only renewable providers;
//  4. order the rest: providers whose guarantee covers MinGuaranteedHours
//     first (when set), then by the preference, then in DeterministicFallback
//     order (lowest cost, lowest carbon, most free capacity).
//
// Full providers are kept (the caller's fit check skips them), so a request
// whose place is full waits instead of moving elsewhere. rtts are measured
// round-trip times per provider ID; loc is the consumer's location (nil when
// unknown). The result may be empty: nothing matches the request.
func RankByIntent(groups []brokerapi.NodeGroupView, in Intent, rtts map[string]float64, loc *Location) []string {
	type cand struct {
		id    string
		ng    brokerapi.NodeGroupView
		order int // DeterministicFallback position, the final tie-break
	}
	fallbackOrder := map[string]int{}
	ids := make([]string, 0, len(groups))
	byID := map[string]brokerapi.NodeGroupView{}
	for _, ng := range groups {
		if _, dup := byID[ng.ProviderClusterID]; !dup {
			ids = append(ids, ng.ProviderClusterID)
		}
		byID[ng.ProviderClusterID] = ng
	}
	if fb, ok := DeterministicFallback(groups); ok {
		for i, id := range fb {
			fallbackOrder[id] = i
		}
	}

	cands := make([]cand, 0, len(ids))
	for _, id := range ids {
		ng := byID[id]
		if inPlaces(ng, in.Exclude) {
			continue
		}
		if len(in.Places) > 0 && !inPlaces(ng, in.Places) {
			continue
		}
		if in.RenewableOnly && !ng.Renewable {
			continue
		}
		order, ok := fallbackOrder[id]
		if !ok {
			order = len(fallbackOrder) // full providers after those with room
		}
		cands = append(cands, cand{id: id, ng: ng, order: order})
	}

	covers := func(ng brokerapi.NodeGroupView) bool {
		return in.MinGuaranteedHours <= 0 || float64(ng.GuaranteedSeconds) >= in.MinGuaranteedHours*3600
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if ca, cb := covers(a.ng), covers(b.ng); ca != cb {
			return ca
		}
		va, oka := preferenceValue(a.ng, in.Prefer, rtts, loc)
		vb, okb := preferenceValue(b.ng, in.Prefer, rtts, loc)
		if oka != okb {
			return oka // a provider with the value before one without
		}
		if oka && va != vb {
			return va < vb
		}
		if a.order != b.order {
			return a.order < b.order
		}
		return a.id < b.id
	})

	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.id
	}
	return out
}

// preferenceValue is the value a preference orders by, lower first; false
// when the provider does not advertise it or there is no preference.
func preferenceValue(ng brokerapi.NodeGroupView, prefer string, rtts map[string]float64, loc *Location) (float64, bool) {
	switch prefer {
	case PreferLatency:
		if v, ok := rtts[ng.ProviderClusterID]; ok && !math.IsInf(v, 0) && !math.IsNaN(v) {
			return v, true
		}
		// Unmeasured providers are farther than every measured one: order
		// them after, by distance (a very large offset keeps them behind).
		if d, ok := distanceKm(ng, loc); ok {
			return 1e9 + d, true
		}
	case PreferNearest:
		return distanceKm(ng, loc)
	case PreferPrice:
		if ng.Cost != nil {
			return ng.Cost.AsApproximateFloat64(), true
		}
	case PreferPricePerCPU:
		if p := unitPrice(ng.UnitPrices, "cpu"); p != nil {
			return *p, true
		}
	case PreferPricePerGiB:
		if p := unitPrice(ng.UnitPrices, "memory"); p != nil {
			return *p, true
		}
	case PreferGreen:
		if ng.CarbonIntensity != nil {
			return *ng.CarbonIntensity, true
		}
	case PreferGreenForecast:
		if avg := forecastAverage(ng.CarbonForecast, forecastHoursIntent); avg != nil {
			return *avg, true
		}
		if ng.CarbonIntensity != nil {
			return *ng.CarbonIntensity, true
		}
	case PreferCapacity:
		return -float64(ng.MaxSize - ng.CurrentReserved), true
	}
	return 0, false
}

func distanceKm(ng brokerapi.NodeGroupView, loc *Location) (float64, bool) {
	if loc == nil || ng.Topology == nil || (ng.Topology.Latitude == 0 && ng.Topology.Longitude == 0) {
		return 0, false
	}
	return brokerapi.HaversineKm(loc.Latitude, loc.Longitude, ng.Topology.Latitude, ng.Topology.Longitude), true
}

// WithoutPlaces returns the groups whose provider is not in any of places
// (city or region). The rank job uses it to apply a request's "not in X" in
// code, since a small model ignores exclusions.
func WithoutPlaces(groups []brokerapi.NodeGroupView, places []string) []brokerapi.NodeGroupView {
	if len(places) == 0 {
		return groups
	}
	out := make([]brokerapi.NodeGroupView, 0, len(groups))
	for _, ng := range groups {
		if !inPlaces(ng, places) {
			out = append(out, ng)
		}
	}
	return out
}

// inPlaces reports whether the provider's city or region is one of places.
// A place matches when the city or region appears in it as a whole word, so
// a model's "in Frankfurt" or "outside Frankfurt" still matches Frankfurt.
func inPlaces(ng brokerapi.NodeGroupView, places []string) bool {
	if ng.Topology == nil {
		return false
	}
	for _, p := range places {
		for _, name := range []string{ng.Topology.City, ng.Topology.Region} {
			if name != "" && (strings.EqualFold(p, name) || placeRe(name).MatchString(p)) {
				return true
			}
		}
	}
	return false
}
