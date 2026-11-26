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

import "testing"

func f(v float64) *float64 { return &v }
func b(v bool) *bool       { return &v }

func TestParseOffer(t *testing.T) {
	s := ParseOffer("We sell CPU at 0.02 euro per core and memory at €0.004 per GB, all solar powered; lend half of our memory and 80% of our CPUs.")
	if s.CPUPrice == nil || *s.CPUPrice != 0.02 {
		t.Errorf("cpu price = %v; want 0.02", s.CPUPrice)
	}
	if s.MemoryPrice == nil || *s.MemoryPrice != 0.004 {
		t.Errorf("memory price = %v; want 0.004", s.MemoryPrice)
	}
	if s.Renewable == nil || !*s.Renewable {
		t.Errorf("renewable = %v; want true", s.Renewable)
	}
	if s.MemorySharePercent == nil || *s.MemorySharePercent != 50 {
		t.Errorf("memory share = %v; want 50", s.MemorySharePercent)
	}
	if s.CPUSharePercent == nil || *s.CPUSharePercent != 80 {
		t.Errorf("cpu share = %v; want 80", s.CPUSharePercent)
	}
	if s.GPUPrice != nil {
		t.Errorf("gpu price = %v; want none", *s.GPUPrice)
	}

	if s := ParseOffer("1.5 EUR per GPU, energy is not renewable"); s.GPUPrice == nil || *s.GPUPrice != 1.5 ||
		s.Renewable == nil || *s.Renewable {
		t.Errorf("ParseOffer = %s; want GPU 1.5 and renewable false", s)
	}
}

func TestCheckOfferDropsWhatTheTextDoesNotSay(t *testing.T) {
	text := "CPU at 0.03 euro per core, green energy, lend half of our memory"
	r := offerResponse{
		CPUPrice:           f(0.03),
		MemoryPrice:        f(0.005), // invented: not in the text
		Renewable:          b(true),
		MemorySharePercent: f(50),  // "half"
		CPUSharePercent:    f(150), // out of range
	}
	s := checkOffer(r, text)
	if s.CPUPrice == nil || *s.CPUPrice != 0.03 || s.MemoryPrice != nil || s.CPUSharePercent != nil {
		t.Fatalf("checkOffer = %s; want only the CPU price, renewable and the memory share", s)
	}
	if s.Renewable == nil || !*s.Renewable || s.MemorySharePercent == nil || *s.MemorySharePercent != 50 {
		t.Fatalf("checkOffer = %s; want renewable true and memory share 50", s)
	}

	// "renewable" needs a word about energy in the text.
	if s := checkOffer(offerResponse{Renewable: b(true)}, "CPU at 0.03 euro per core"); s.Renewable != nil {
		t.Fatalf("renewable = %v; want unset", *s.Renewable)
	}
}
