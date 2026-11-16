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
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
}

// requestView is what the panel shows: what was asked, what was understood,
// and how it is going.
type requestView struct {
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
}

// requestMemo is the last request the panel sent (process memory: after an
// agent restart the panel still finds the live objects, without the text).
type requestMemo struct {
	mu   sync.Mutex
	last *requestView
}

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
		if err := s.releaseRequest(r.Context()); err != nil {
			s.writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.ok(w)
	default:
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown action %q (want send|release)", body.Action))
	}
}

// handleRequestState serves GET /api/request: the current request and its
// progress, polled by the panel.
func (s *Server) handleRequestState(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, s.requestState(r.Context()))
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
	if active, err := s.requestActive(ctx); err != nil {
		return nil, http.StatusInternalServerError, err
	} else if active {
		return nil, http.StatusConflict, fmt.Errorf("a request is already active: release it first")
	}

	amount, source, err := s.understandAmount(ctx, text, body)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	resources, nodes, block, err := sizeRequest(amount)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}

	var bound *autoscalingv1alpha1.LatencyBound
	if amount.MaxLatencyMs > 0 {
		bound = &autoscalingv1alpha1.LatencyBound{MaxLatencyMs: amount.MaxLatencyMs}
	}
	switch body.Mode {
	case RequestModeReserve:
		err = s.createRequestReservation(ctx, text, resources, bound)
	case RequestModeOnDemand:
		err = s.applyOnDemand(ctx, text, nodes, block, bound)
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}

	view := &requestView{
		Active:     true,
		Text:       text,
		Mode:       body.Mode,
		Understood: amount.String(),
		Source:     source,
		Nodes:      nodes,
		Resources:  resourceListToMap(resources),
		SentAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if !amount.HasSize() {
		view.Understood = "1 node (no amount stated)"
		if amount.MaxLatencyMs > 0 {
			view.Understood += fmt.Sprintf(", max %d ms", amount.MaxLatencyMs)
		}
	}
	view.MaxLatencyMs = amount.MaxLatencyMs
	s.request.mu.Lock()
	s.request.last = view
	s.request.mu.Unlock()
	s.log.Info("plain-language request sent",
		"mode", body.Mode, "text", text, "understood", view.Understood, "source", source, "nodes", nodes)
	return view, 0, nil
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

// createRequestReservation creates the Reserve now ResourceRequest, with the
// latency limit read from the text when there is one.
func (s *Server) createRequestReservation(
	ctx context.Context, text string, res corev1.ResourceList, bound *autoscalingv1alpha1.LatencyBound,
) error {
	rr := &autoscalingv1alpha1.ResourceRequest{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: requestPrefix,
			Namespace:    s.ns,
			Labels:       map[string]string{consoleManagedLabel: requestLabelValue},
		},
		Spec: autoscalingv1alpha1.ResourceRequestSpec{
			Resources:    res,
			UserPrompt:   text,
			LatencyBound: bound,
		},
	}
	if err := s.local.Create(ctx, rr); err != nil {
		return fmt.Errorf("create the reservation: %w", err)
	}
	return nil
}

// applyOnDemand sets the ConsumerPolicy to ConsumerChoice with the text (and
// the latency limit read from it, if any) and applies a workload of one pod
// per node, pinned to borrowed nodes.
func (s *Server) applyOnDemand(
	ctx context.Context, text string, nodes int32, block corev1.ResourceList, bound *autoscalingv1alpha1.LatencyBound,
) error {
	if err := s.upsertConsumerPolicy(ctx, autoscalingv1alpha1.PlacementStrategyConsumerChoice, text); err != nil {
		return fmt.Errorf("set the ConsumerChoice policy: %w", err)
	}
	if err := s.setPolicyBound(ctx, bound); err != nil {
		return fmt.Errorf("set the latency limit: %w", err)
	}
	if err := s.local.Create(ctx, onDemandDeployment(nodes, block)); err != nil {
		return fmt.Errorf("apply the workload: %w", err)
	}
	return nil
}

// onDemandDeployment is the On demand workload: nodes pause pods, each
// requesting podShareOfNode of one node and allowed only on borrowed (Liqo
// virtual) nodes, so the Cluster Autoscaler borrows exactly nodes nodes.
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

// releaseRequest removes whatever the panel placed: its ResourceRequests and
// the On demand workload. Missing objects are fine.
func (s *Server) releaseRequest(ctx context.Context) error {
	var list autoscalingv1alpha1.ResourceRequestList
	if err := s.local.List(ctx, &list, ctrlclient.InNamespace(s.ns),
		ctrlclient.MatchingLabels{consoleManagedLabel: requestLabelValue}); err != nil {
		return fmt.Errorf("list requests: %w", err)
	}
	for i := range list.Items {
		if err := s.local.Delete(ctx, &list.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("release %s: %w", list.Items[i].Name, err)
		}
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: onDemandWorkload, Namespace: workloadNamespace}}
	err := s.local.Delete(ctx, dep)
	switch {
	case err == nil:
		// The On demand latency limit lives on the ConsumerPolicy; drop it so
		// it does not constrain later autoscaler decisions.
		if err := s.setPolicyBound(ctx, nil); err != nil {
			return fmt.Errorf("clear the latency limit: %w", err)
		}
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("delete the workload: %w", err)
	}
	s.request.mu.Lock()
	if s.request.last != nil {
		s.request.last.Active = false
		s.request.last.Status = "released"
	}
	s.request.mu.Unlock()
	s.log.Info("plain-language request released")
	return nil
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

// requestActive reports whether a panel request is still placed.
func (s *Server) requestActive(ctx context.Context) (bool, error) {
	rr, err := s.panelReservation(ctx)
	if err != nil {
		return false, err
	}
	if rr != nil {
		return true, nil
	}
	dep, err := s.onDemand(ctx)
	return dep != nil, err
}

// panelReservation returns the panel's ResourceRequest still alive (not
// being deleted), or nil.
func (s *Server) panelReservation(ctx context.Context) (*autoscalingv1alpha1.ResourceRequest, error) {
	var list autoscalingv1alpha1.ResourceRequestList
	if err := s.local.List(ctx, &list, ctrlclient.InNamespace(s.ns),
		ctrlclient.MatchingLabels{consoleManagedLabel: requestLabelValue}); err != nil {
		return nil, fmt.Errorf("list requests: %w", err)
	}
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp.IsZero() {
			return &list.Items[i], nil
		}
	}
	return nil, nil
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

// requestState is the panel's view: the last request plus live progress.
func (s *Server) requestState(ctx context.Context) requestView {
	s.request.mu.Lock()
	var view requestView
	if s.request.last != nil {
		view = *s.request.last
	}
	s.request.mu.Unlock()

	if rr, err := s.panelReservation(ctx); err == nil && rr != nil {
		view.Active = true
		view.Mode = RequestModeReserve
		if view.Text == "" {
			view.Text = rr.Spec.UserPrompt
		}
		view.Phase = string(rr.Status.Phase)
		view.Provider = rr.Status.ProviderClusterID
		view.Status = rr.Status.Message
		if view.Status == "" {
			view.Status = "submitted"
		}
		return view
	}
	if dep, err := s.onDemand(ctx); err == nil && dep != nil {
		view.Active = true
		view.Mode = RequestModeOnDemand
		want := int32(0)
		if dep.Spec.Replicas != nil {
			want = *dep.Spec.Replicas
		}
		view.Nodes = want
		view.Ready = dep.Status.ReadyReplicas
		view.Status = fmt.Sprintf("%d of %d pods running on borrowed nodes", view.Ready, want)
		return view
	}
	if view.Active { // placed earlier, now gone
		view.Active = false
		view.Status = "released"
	}
	return view
}
