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

package console

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
)

// offerTextTimeout bounds one reading of an offer by the AI.
const offerTextTimeout = 60 * time.Second

// offerTextResponse is what POST /api/offer-text returns.
type offerTextResponse struct {
	// Understood is the reading in short, e.g. "CPU 0.02 EUR/core/h".
	Understood string `json:"understood"`
	// Settings is the reading, field by field.
	Settings ollama.OfferSettings `json:"settings"`
	// Source is "ai" (the model) or "text" (fixed patterns).
	Source string `json:"source"`
	// AIError is why the model's reading was not used, if it was not.
	AIError string `json:"aiError,omitempty"`
	// Applied is true when the settings were written.
	Applied bool `json:"applied"`
}

// handleOfferText reads an offer written in plain words (the AI on the
// provider, phase 1) into the provider's settings: prices, renewable energy
// and how much to lend. With "apply" it writes them into the same ConfigMaps
// the console's own fields write (only what the text states; the rest is
// kept), and the next advertisement carries them. Provider-only.
func (s *Server) handleOfferText(w http.ResponseWriter, r *http.Request) {
	if s.role != RoleProvider {
		s.writeError(w, http.StatusMethodNotAllowed, "the offer text is a provider-only setting")
		return
	}
	var body struct {
		Text  string `json:"text"`
		Apply bool   `json:"apply"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		s.writeError(w, http.StatusBadRequest, "text is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), offerTextTimeout)
	defer cancel()
	res := ollama.UnderstandOffer(ctx, s.ollama, text)
	out := offerTextResponse{
		Understood: res.Settings.String(),
		Settings:   res.Settings,
		Source:     res.Source,
		AIError:    res.AIError,
	}
	if body.Apply && !res.Settings.Empty() {
		if err := s.applyOffer(r.Context(), res.Settings); err != nil {
			s.writeError(w, http.StatusInternalServerError, "apply the offer: "+err.Error())
			return
		}
		out.Applied = true
	}
	s.writeJSON(w, http.StatusOK, out)
}

// applyOffer writes the settings the text stated, keeping the others.
func (s *Server) applyOffer(ctx context.Context, set ollama.OfferSettings) error {
	prices := map[string]any{}
	price := func(name string, v *float64) {
		if v != nil {
			prices[name] = strconv.FormatFloat(*v, 'f', -1, 64)
		}
	}
	price("cpu", set.CPUPrice)
	price("memory", set.MemoryPrice)
	price("nvidia.com/gpu", set.GPUPrice)
	if err := s.mergeYAML(ctx, pricesConfigMap, pricesKey, prices); err != nil {
		return fmt.Errorf("write %s: %w", pricesConfigMap, err)
	}

	shares := map[string]any{}
	if set.CPUSharePercent != nil {
		shares["cpu"] = *set.CPUSharePercent
	}
	if set.MemorySharePercent != nil {
		shares["memory"] = *set.MemorySharePercent
	}
	if err := s.mergeYAML(ctx, capacityConfigMap, capacityKey, shares); err != nil {
		return fmt.Errorf("write %s: %w", capacityConfigMap, err)
	}

	if set.Renewable != nil {
		value := fmt.Sprintf("renewable: %t\n", *set.Renewable)
		if err := s.upsertConfigMap(ctx, renewableConfigMap, renewableKey, value); err != nil {
			return fmt.Errorf("write %s: %w", renewableConfigMap, err)
		}
	}
	return nil
}

// mergeYAML sets the given entries in the YAML map stored at data[key] of a
// ConfigMap, keeping its other entries. Nothing to set: nothing is written.
func (s *Server) mergeYAML(ctx context.Context, name, key string, entries map[string]any) error {
	if len(entries) == 0 {
		return nil
	}
	current := map[string]any{}
	var cm corev1.ConfigMap
	err := s.local.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: name}, &cm)
	switch {
	case err == nil:
		if raw := strings.TrimSpace(cm.Data[key]); raw != "" {
			if err := yaml.Unmarshal([]byte(raw), &current); err != nil {
				return fmt.Errorf("read the current %s: %w", key, err)
			}
		}
	case !apierrors.IsNotFound(err):
		return err
	}
	for k, v := range entries {
		current[k] = v
	}
	data, err := yaml.Marshal(current)
	if err != nil {
		return err
	}
	return s.upsertConfigMap(ctx, name, key, string(data))
}
