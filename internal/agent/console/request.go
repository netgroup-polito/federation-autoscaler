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
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
	"github.com/netgroup-polito/federation-autoscaler/internal/broker/chunk"
)

// The plain-language request panel: one text, an optional amount, and a mode.
//
//   - Reserve now (RequestModeReserve): one ResourceRequest carrying the text;
//     the local AI ranks the providers for it and the capacity is reserved at
//     once. No Cluster Autoscaler involved.
//   - On demand (RequestModeOnDemand): the ConsumerPolicy becomes ConsumerChoice
//     with the text, and a workload of one pod per node is applied, pinned to
//     borrowed nodes; the Cluster Autoscaler borrows the same nodes.
//
// Both end with the same borrowed nodes on the provider the AI chose.
const (
	RequestModeReserve  = "reserve"
	RequestModeOnDemand = "ondemand"

	// maxRequestNodes caps one request, the same limit as a ResourceRequest.
	maxRequestNodes = 4
	// maxRequestTextLen matches the CRD limit on ResourceRequest.spec.userPrompt.
	maxRequestTextLen = 2048
	// understandTimeout bounds the AI call that reads the amount.
	understandTimeout = 60 * time.Second

	// requestPrefix / requestLabelValue name and tag the ResourceRequests the
	// panel creates (consoleManagedLabel = requestLabelValue), apart from the
	// manual-reservation panel's.
	requestPrefix     = "request-"
	requestLabelValue = "request"

	// onDemandWorkload is the Deployment the On demand mode applies.
	onDemandWorkload = "federation-request"
	// podShareOfNode is how much of one node each on-demand pod requests:
	// more than half, so exactly one pod fits per borrowed node.
	podShareOfNode = 0.6
)

// requestBody is the POST /api/request body.
type requestBody struct {
	Action string `json:"action"` // "send" | "release"
	Text   string `json:"text"`
	Mode   string `json:"mode"`
	// Optional amount, overriding what the AI reads from the text.
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	GPU    string `json:"gpu,omitempty"`
	// Name selects the request to release; empty releases all of them.
	Name string `json:"name,omitempty"`
	// Group releases every part of one split request (see annoGroupText).
	Group string `json:"group,omitempty"`
}

// requestView is one placed request as the panel shows it: what was asked,
// what was understood, and how it is going.
type requestView struct {
	// Name identifies the request for Release: the ResourceRequest's name
	// (Reserve now) or onDemandName (On demand).
	Name       string            `json:"name"`
	Active     bool              `json:"active"`
	Text       string            `json:"text,omitempty"`
	Mode       string            `json:"mode,omitempty"`
	Understood string            `json:"understood,omitempty"` // e.g. "4 CPU, 8Gi memory"
	Source     string            `json:"source,omitempty"`     // ai | text | form | default
	Nodes      int32             `json:"nodes,omitempty"`
	Resources  map[string]string `json:"resources,omitempty"`
	// MaxLatencyMs is the latency limit read from the request (0: none).
	MaxLatencyMs int32 `json:"maxLatencyMs,omitempty"`
	// Status is a one-line progress summary; Phase/Provider are set in
	// Reserve now mode, Ready in On demand mode.
	Status   string `json:"status,omitempty"`
	Phase    string `json:"phase,omitempty"`
	Provider string `json:"provider,omitempty"`
	Ready    int32  `json:"ready,omitempty"`
	SentAt   string `json:"sentAt,omitempty"`
	// Group, GroupText and Part are set on each part of a request that was
	// split by place: the group's ID, the whole request, and "1/2".
	Group     string `json:"group,omitempty"`
	GroupText string `json:"groupText,omitempty"`
	Part      string `json:"part,omitempty"`
	// Parts is how many requests a send created (more than 1 when split).
	Parts int `json:"parts,omitempty"`
}

// requestList is the GET /api/request body: every placed request, oldest
// first.
type requestList struct {
	Requests []requestView `json:"requests"`
}

// What a request understood is kept on its own object (annotations on the
// ResourceRequest, or on the On demand Deployment), so the panel's list
// survives a page reload and an agent restart.
const (
	annoText       = "federation-autoscaler.io/request-text"
	annoUnderstood = "federation-autoscaler.io/request-understood"
	annoSource     = "federation-autoscaler.io/request-source"
	annoNodes      = "federation-autoscaler.io/request-nodes"
	annoMaxLatency = "federation-autoscaler.io/request-max-latency-ms"
	// A request asking for several places ("2 VMs in Turin and 1 VM in
	// Frankfurt") becomes one ResourceRequest per place, linked by the
	// groupLabel; each part keeps the whole request and its position.
	groupLabel    = "federation-autoscaler.io/request-group"
	annoGroupText = "federation-autoscaler.io/request-group-text"
	annoPart      = "federation-autoscaler.io/request-part"

	// onDemandName names the On demand request in the panel's list.
	onDemandName = "on-demand"
)

// handleRequest serves POST /api/request.
func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	var body requestBody
	if !s.decode(w, r, &body) {
		return
	}
	switch body.Action {
	case "send":
		view, status, err := s.sendRequest(r.Context(), body)
		if err != nil {
			s.writeError(w, status, err.Error())
			return
		}
		s.writeJSON(w, http.StatusOK, view)
	case "release":
		if body.Group != "" {
			status, err := s.releaseGroup(r.Context(), body.Group)
			if err != nil {
				s.writeError(w, status, err.Error())
				return
			}
			s.ok(w)
			return
		}
		status, err := s.releaseRequest(r.Context(), body.Name)
		if err != nil {
			s.writeError(w, status, err.Error())
			return
		}
		s.ok(w)
	default:
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown action %q (want send|release)", body.Action))
	}
}

// handleRequestState serves GET /api/request: every placed request and its
// progress, polled by the panel.
func (s *Server) handleRequestState(w http.ResponseWriter, r *http.Request) {
	list, err := s.listRequests(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}

// sendRequest understands, sizes and places one request. It returns the HTTP
// status to answer with on error.
func (s *Server) sendRequest(ctx context.Context, body requestBody) (*requestView, int, error) {
	text := strings.TrimSpace(body.Text)
	switch {
	case text == "":
		return nil, http.StatusBadRequest, fmt.Errorf("write a request first")
	case len(text) > maxRequestTextLen:
		return nil, http.StatusBadRequest, fmt.Errorf("the request is longer than %d characters", maxRequestTextLen)
	case body.Mode != RequestModeReserve && body.Mode != RequestModeOnDemand:
		return nil, http.StatusBadRequest, fmt.Errorf("unknown mode %q (want %s|%s)", body.Mode, RequestModeReserve, RequestModeOnDemand)
	}
	if body.Mode == RequestModeOnDemand {
		// On demand works through the consumer's single policy text, so only
		// one can be active; Reserve now requests can be many.
		if dep, err := s.onDemand(ctx); err != nil {
			return nil, http.StatusInternalServerError, err
		} else if dep != nil {
			return nil, http.StatusConflict, fmt.Errorf("an On demand request is already active: release it first")
		}
	}

	// Reserve now: a request asking for several places becomes one request
	// per place. The amount fields describe one request, so with them filled
	// the text is never split.
	if body.Mode == RequestModeReserve && !formAmount(body) {
		uctx, cancel := context.WithTimeout(ctx, understandTimeout)
		split := ollama.UnderstandSplit(uctx, s.ollama, text)
		cancel()
		if split.AIError != "" {
			s.log.Info("AI could not split the request; kept whole", "err", split.AIError)
		}
		if len(split.Parts) > 1 {
			return s.sendSplit(ctx, text, split.Parts)
		}
	}

	p, err := s.prepare(ctx, text, body)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}

	name := onDemandName
	switch body.Mode {
	case RequestModeReserve:
		name, err = s.createRequestReservation(ctx, text, p.resources, p.bound, p.annos, nil)
	case RequestModeOnDemand:
		err = s.applyOnDemand(ctx, text, p.nodes, p.block, p.bound, p.annos)
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}

	view := p.view(name, text, body.Mode)
	s.log.Info("plain-language request sent",
		"name", name, "mode", body.Mode, "text", text, "understood", p.understood, "source", p.source, "nodes", p.nodes)
	return view, 0, nil
}

// prepared is one request understood and sized, ready to be placed.
type prepared struct {
	amount     ollama.Amount
	source     string
	understood string
	resources  corev1.ResourceList
	nodes      int32
	block      corev1.ResourceList
	bound      *autoscalingv1alpha1.LatencyBound
	annos      map[string]string
}

// prepare reads the amount and latency limit from text (and the panel's
// amount fields), sizes it into nodes and builds its annotations.
func (s *Server) prepare(ctx context.Context, text string, body requestBody) (*prepared, error) {
	amount, source, err := s.understandAmount(ctx, text, body)
	if err != nil {
		return nil, err
	}
	resources, nodes, block, err := sizeRequest(amount)
	if err != nil {
		return nil, err
	}
	p := &prepared{amount: amount, source: source, resources: resources, nodes: nodes, block: block}
	if amount.MaxLatencyMs > 0 {
		p.bound = &autoscalingv1alpha1.LatencyBound{MaxLatencyMs: amount.MaxLatencyMs}
	}
	p.understood = amount.String()
	if !amount.HasSize() {
		p.understood = "1 node (no amount stated)"
		if amount.MaxLatencyMs > 0 {
			p.understood += fmt.Sprintf(", max %d ms", amount.MaxLatencyMs)
		}
	}
	p.annos = map[string]string{
		annoText:       text,
		annoUnderstood: p.understood,
		annoSource:     source,
		annoNodes:      strconv.Itoa(int(nodes)),
	}
	if p.bound != nil {
		p.annos[annoMaxLatency] = strconv.Itoa(int(p.bound.MaxLatencyMs))
	}
	return p, nil
}

// view is the panel's view of a request just placed.
func (p *prepared) view(name, text, mode string) *requestView {
	return &requestView{
		Name:         name,
		Active:       true,
		Text:         text,
		Mode:         mode,
		Understood:   p.understood,
		Source:       p.source,
		Nodes:        p.nodes,
		Resources:    resourceListToMap(p.resources),
		MaxLatencyMs: p.amount.MaxLatencyMs,
		Status:       "submitted",
		SentAt:       time.Now().UTC().Format(time.RFC3339),
		Parts:        1,
	}
}

// formAmount reports whether any of the panel's amount fields is filled.
func formAmount(body requestBody) bool {
	return strings.TrimSpace(body.CPU) != "" || strings.TrimSpace(body.Memory) != "" || strings.TrimSpace(body.GPU) != ""
}

// sendSplit places one Reserve now request per part of a request split by
// place, linked by one group. Every part is understood and sized first, so a
// part that cannot be placed rejects the whole request before anything is
// created; if creating a part fails, the parts already created are removed.
func (s *Server) sendSplit(ctx context.Context, text string, parts []string) (*requestView, int, error) {
	preps := make([]*prepared, len(parts))
	for i, part := range parts {
		p, err := s.prepare(ctx, part, requestBody{})
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("part %q: %w", part, err)
		}
		preps[i] = p
	}

	group := "g-" + utilrand.String(8)
	created := make([]string, 0, len(preps))
	for i, p := range preps {
		p.annos[annoGroupText] = text
		p.annos[annoPart] = fmt.Sprintf("%d/%d", i+1, len(parts))
		name, err := s.createRequestReservation(ctx, parts[i], p.resources, p.bound, p.annos,
			map[string]string{groupLabel: group})
		if err != nil {
			if _, rerr := s.releaseGroup(ctx, group); rerr != nil {
				s.log.Info("could not remove the parts already created", "group", group, "err", rerr)
			}
			return nil, http.StatusInternalServerError, err
		}
		created = append(created, name)
	}

	view := preps[0].view(created[0], parts[0], RequestModeReserve)
	view.Group, view.GroupText, view.Part, view.Parts = group, text, fmt.Sprintf("1/%d", len(parts)), len(parts)
	s.log.Info("plain-language request split by place and sent",
		"text", text, "parts", parts, "names", created, "group", group)
	return view, 0, nil
}

// releaseGroup removes every part of one split request.
func (s *Server) releaseGroup(ctx context.Context, group string) (int, error) {
	var list autoscalingv1alpha1.ResourceRequestList
	if err := s.local.List(ctx, &list, ctrlclient.InNamespace(s.ns),
		ctrlclient.MatchingLabels{consoleManagedLabel: requestLabelValue, groupLabel: group}); err != nil {
		return http.StatusInternalServerError, fmt.Errorf("list requests: %w", err)
	}
	if len(list.Items) == 0 {
		return http.StatusNotFound, fmt.Errorf("no request group %q", group)
	}
	for i := range list.Items {
		if err := s.local.Delete(ctx, &list.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return http.StatusInternalServerError, fmt.Errorf("release %s: %w", list.Items[i].Name, err)
		}
	}
	s.log.Info("plain-language request group released", "group", group, "parts", len(list.Items))
	return 0, nil
}

// understandAmount reads the amount from the text (AI, else fixed patterns)
// and applies the panel's optional fields on top. source is "ai", "text",
// "form" (a field was filled) or "default" (nothing stated anywhere).
func (s *Server) understandAmount(ctx context.Context, text string, body requestBody) (ollama.Amount, string, error) {
	uctx, cancel := context.WithTimeout(ctx, understandTimeout)
	defer cancel()
	res := ollama.UnderstandAmount(uctx, s.ollama, text)
	amount, source := res.Amount, res.Source
	if res.AIError != "" {
		s.log.Info("AI could not read the amount; read from the text instead", "err", res.AIError)
	}

	override := func(raw string, set func(resource.Quantity)) error {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil
		}
		q, err := resource.ParseQuantity(raw)
		if err != nil || q.Sign() <= 0 {
			return fmt.Errorf("%q is not a positive amount", raw)
		}
		set(q)
		source = "form"
		return nil
	}
	if err := override(body.CPU, func(q resource.Quantity) { amount.CPU = q }); err != nil {
		return amount, "", err
	}
	if err := override(body.Memory, func(q resource.Quantity) { amount.Memory = q }); err != nil {
		return amount, "", err
	}
	if err := override(body.GPU, func(q resource.Quantity) { amount.GPU = q.Value() }); err != nil {
		return amount, "", err
	}
	if amount.IsZero() {
		source = "default"
	}
	return amount, source, nil
}

// blockSizes returns the size of one standard node and one GPU node, as the
// Broker's sizer cuts them.
func blockSizes() (standard, gpu corev1.ResourceList) {
	sizer := chunk.NewDefaultSizer()
	big := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1000"),
		corev1.ResourceMemory: resource.MustParse("4000Gi"),
	}
	if rs := sizer.Size(big); len(rs) > 0 {
		standard = rs[0].PerChunk
	}
	big[chunk.GPUResourceName] = resource.MustParse("100")
	if rs := sizer.Size(big); len(rs) > 0 {
		gpu = rs[0].PerChunk
	}
	return standard, gpu
}

// sizeRequest turns an amount into the resources to reserve, the number of
// nodes, and the size of one node. No amount means one node; a node count
// means that many whole nodes (raised to any larger stated resource).
func sizeRequest(a ollama.Amount) (corev1.ResourceList, int32, corev1.ResourceList, error) {
	standard, gpu := blockSizes()
	block := standard
	if a.GPU > 0 {
		block = gpu
	}
	var res corev1.ResourceList
	switch {
	case !a.HasSize():
		res = block.DeepCopy()
	case a.Nodes > 0:
		res = corev1.ResourceList{}
		for name, q := range block {
			total := q.DeepCopy()
			total.Set(0)
			for i := int64(0); i < a.Nodes; i++ {
				total.Add(q)
			}
			res[name] = total
		}
		for name, q := range a.ResourceList() {
			if cur, ok := res[name]; !ok || q.Cmp(cur) > 0 {
				res[name] = q
			}
		}
	default:
		res = a.ResourceList()
	}
	nodes := chunk.CountFor(res, block)
	if nodes <= 0 {
		return nil, 0, nil, fmt.Errorf("cannot size %s into nodes", a.String())
	}
	if nodes > maxRequestNodes {
		return nil, 0, nil, fmt.Errorf("that needs %d nodes; at most %d per request", nodes, maxRequestNodes)
	}
	return res, nodes, block, nil
}

// createRequestReservation creates one Reserve now ResourceRequest, with the
// latency limit read from the text when there is one and what was understood
// as annotations, and returns its name.
func (s *Server) createRequestReservation(
	ctx context.Context, text string, res corev1.ResourceList,
	bound *autoscalingv1alpha1.LatencyBound, annos, extraLabels map[string]string,
) (string, error) {
	labels := map[string]string{consoleManagedLabel: requestLabelValue}
	for k, v := range extraLabels {
		labels[k] = v
	}
	rr := &autoscalingv1alpha1.ResourceRequest{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: requestPrefix,
			Namespace:    s.ns,
			Labels:       labels,
			Annotations:  annos,
		},
		Spec: autoscalingv1alpha1.ResourceRequestSpec{
			Resources:    res,
			UserPrompt:   text,
			LatencyBound: bound,
		},
	}
	if err := s.local.Create(ctx, rr); err != nil {
		return "", fmt.Errorf("create the reservation: %w", err)
	}
	return rr.Name, nil
}

// applyOnDemand sets the ConsumerPolicy to ConsumerChoice with the text (and
// the latency limit read from it, if any) and applies a workload of one pod
// per node, pinned to borrowed nodes.
func (s *Server) applyOnDemand(
	ctx context.Context, text string, nodes int32, block corev1.ResourceList,
	bound *autoscalingv1alpha1.LatencyBound, annos map[string]string,
) error {
	if err := s.upsertConsumerPolicy(ctx, autoscalingv1alpha1.PlacementStrategyConsumerChoice, text); err != nil {
		return fmt.Errorf("set the ConsumerChoice policy: %w", err)
	}
	if err := s.setPolicyBound(ctx, bound); err != nil {
		return fmt.Errorf("set the latency limit: %w", err)
	}
	dep := onDemandDeployment(nodes, block)
	dep.Annotations = annos
	if err := s.local.Create(ctx, dep); err != nil {
		return fmt.Errorf("apply the workload: %w", err)
	}
	return nil
}

// onDemandDeployment is the On demand workload: nodes pause pods, each
// requesting podShareOfNode of one node and allowed only on borrowed (Liqo
// virtual) nodes not held for a Reserve now request, so the Cluster
// Autoscaler borrows exactly nodes nodes.
func onDemandDeployment(nodes int32, block corev1.ResourceList) *appsv1.Deployment {
	requests := corev1.ResourceList{}
	for name, q := range block {
		if name == chunk.GPUResourceName {
			requests[name] = q.DeepCopy() // GPUs are whole
			continue
		}
		requests[name] = *resource.NewMilliQuantity(int64(float64(q.MilliValue())*podShareOfNode), q.Format)
	}
	labels := map[string]string{"app": onDemandWorkload}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: onDemandWorkload, Namespace: workloadNamespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &nodes,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{"liqo.io/type": "virtual-node"},
					// Never on a node held for a Reserve now request: that
					// capacity belongs to the other request (and may not meet
					// this request's latency limit).
					Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
							NodeSelectorTerms: []corev1.NodeSelectorTerm{{
								MatchExpressions: []corev1.NodeSelectorRequirement{{
									Key: autoscalingv1alpha1.HeldByLabel, Operator: corev1.NodeSelectorOpDoesNotExist,
								}},
							}},
						},
					}},
					Tolerations: []corev1.Toleration{{
						Key: "virtual-node.liqo.io/not-allowed", Operator: corev1.TolerationOpExists,
						Effect: corev1.TaintEffectNoExecute,
					}},
					Containers: []corev1.Container{{
						Name:      "pause",
						Image:     "registry.k8s.io/pause:3.9",
						Resources: corev1.ResourceRequirements{Requests: requests, Limits: requests},
					}},
				},
			},
		},
	}
}

// releaseRequest removes one placed request by name (a ResourceRequest's
// name, or onDemandName), or every one of them when name is empty. It returns
// the HTTP status to answer with on error.
func (s *Server) releaseRequest(ctx context.Context, name string) (int, error) {
	if name == "" || name != onDemandName {
		var list autoscalingv1alpha1.ResourceRequestList
		if err := s.local.List(ctx, &list, ctrlclient.InNamespace(s.ns),
			ctrlclient.MatchingLabels{consoleManagedLabel: requestLabelValue}); err != nil {
			return http.StatusInternalServerError, fmt.Errorf("list requests: %w", err)
		}
		found := false
		for i := range list.Items {
			if name != "" && list.Items[i].Name != name {
				continue
			}
			found = true
			if err := s.local.Delete(ctx, &list.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return http.StatusInternalServerError, fmt.Errorf("release %s: %w", list.Items[i].Name, err)
			}
		}
		if name != "" {
			if !found {
				return http.StatusNotFound, fmt.Errorf("no request named %q", name)
			}
			s.log.Info("plain-language request released", "name", name)
			return 0, nil
		}
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: onDemandWorkload, Namespace: workloadNamespace}}
	err := s.local.Delete(ctx, dep)
	switch {
	case err == nil:
		// The On demand latency limit lives on the ConsumerPolicy; drop it so
		// it does not constrain later autoscaler decisions.
		if err := s.setPolicyBound(ctx, nil); err != nil {
			return http.StatusInternalServerError, fmt.Errorf("clear the latency limit: %w", err)
		}
	case apierrors.IsNotFound(err):
		if name == onDemandName {
			return http.StatusNotFound, fmt.Errorf("no On demand request is active")
		}
	default:
		return http.StatusInternalServerError, fmt.Errorf("delete the workload: %w", err)
	}
	s.log.Info("plain-language request released", "name", name)
	return 0, nil
}

// setPolicyBound sets (or, with nil, clears) the ConsumerPolicy's latency
// limit. A missing policy is fine when clearing.
func (s *Server) setPolicyBound(ctx context.Context, bound *autoscalingv1alpha1.LatencyBound) error {
	var cp autoscalingv1alpha1.ConsumerPolicy
	err := s.local.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: consumerPolicyName}, &cp)
	if apierrors.IsNotFound(err) && bound == nil {
		return nil
	}
	if err != nil {
		return err
	}
	if (cp.Spec.LatencyBound == nil) == (bound == nil) &&
		(bound == nil || cp.Spec.LatencyBound.MaxLatencyMs == bound.MaxLatencyMs) {
		return nil
	}
	cp.Spec.LatencyBound = bound
	return s.local.Update(ctx, &cp)
}

// onDemand returns the On demand workload when it exists (not being deleted).
func (s *Server) onDemand(ctx context.Context) (*appsv1.Deployment, error) {
	var dep appsv1.Deployment
	err := s.local.Get(ctx, types.NamespacedName{Namespace: workloadNamespace, Name: onDemandWorkload}, &dep)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read the workload: %w", err)
	case !dep.DeletionTimestamp.IsZero():
		return nil, nil
	}
	return &dep, nil
}

// listRequests returns every placed request (oldest first; the On demand one
// last) with its live progress.
func (s *Server) listRequests(ctx context.Context) (requestList, error) {
	out := requestList{Requests: []requestView{}}
	var list autoscalingv1alpha1.ResourceRequestList
	if err := s.local.List(ctx, &list, ctrlclient.InNamespace(s.ns),
		ctrlclient.MatchingLabels{consoleManagedLabel: requestLabelValue}); err != nil {
		return out, fmt.Errorf("list requests: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool {
		x, y := &list.Items[i], &list.Items[j]
		// The parts of one split request, in their order.
		if g := x.Labels[groupLabel]; g != "" && g == y.Labels[groupLabel] {
			return partIndex(x.Annotations[annoPart]) < partIndex(y.Annotations[annoPart])
		}
		a, b := x.CreationTimestamp, y.CreationTimestamp
		if a.Equal(&b) {
			return x.Name < y.Name
		}
		return a.Before(&b)
	})
	for i := range list.Items {
		rr := &list.Items[i]
		if !rr.DeletionTimestamp.IsZero() {
			continue // being released
		}
		v := viewFromAnnotations(rr.Annotations)
		v.Name = rr.Name
		v.Active = true
		v.Mode = RequestModeReserve
		v.Text = rr.Spec.UserPrompt
		v.Resources = resourceListToMap(rr.Spec.Resources)
		if rr.Spec.LatencyBound != nil {
			v.MaxLatencyMs = rr.Spec.LatencyBound.MaxLatencyMs
		}
		if v.Nodes == 0 {
			v.Nodes = rr.Status.ChunkCount
		}
		v.Phase = string(rr.Status.Phase)
		v.Provider = rr.Status.ProviderClusterID
		v.Status = rr.Status.Message
		if v.Status == "" {
			v.Status = "submitted"
		}
		v.SentAt = rr.CreationTimestamp.UTC().Format(time.RFC3339)
		v.Group = rr.Labels[groupLabel]
		v.GroupText = rr.Annotations[annoGroupText]
		v.Part = rr.Annotations[annoPart]
		out.Requests = append(out.Requests, v)
	}
	dep, err := s.onDemand(ctx)
	if err != nil {
		return out, err
	}
	if dep != nil {
		v := viewFromAnnotations(dep.Annotations)
		v.Name = onDemandName
		v.Active = true
		v.Mode = RequestModeOnDemand
		v.Text = dep.Annotations[annoText]
		if dep.Spec.Replicas != nil {
			v.Nodes = *dep.Spec.Replicas
		}
		v.Ready = dep.Status.ReadyReplicas
		v.Status = fmt.Sprintf("%d of %d pods running on borrowed nodes", v.Ready, v.Nodes)
		v.SentAt = dep.CreationTimestamp.UTC().Format(time.RFC3339)
		out.Requests = append(out.Requests, v)
	}
	return out, nil
}

// partIndex reads the position from a part annotation ("2/3" -> 2).
func partIndex(part string) int {
	n, _ := strconv.Atoi(strings.SplitN(part, "/", 2)[0])
	return n
}

// viewFromAnnotations fills what a request understood from its annotations.
func viewFromAnnotations(annos map[string]string) requestView {
	v := requestView{Understood: annos[annoUnderstood], Source: annos[annoSource]}
	if n, err := strconv.Atoi(annos[annoNodes]); err == nil {
		v.Nodes = int32(n)
	}
	if ms, err := strconv.Atoi(annos[annoMaxLatency]); err == nil {
		v.MaxLatencyMs = int32(ms)
	}
	return v
}
