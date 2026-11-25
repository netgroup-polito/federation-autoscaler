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

package sites

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

const (
	label = "example.org/site"
	ns    = "fa"
)

func rl(cpu, mem string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
}

func node(name, site, cpu, mem string, ready, schedulable bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	if site != "" {
		n.Labels[label] = site
	}
	n.Spec.Unschedulable = !schedulable
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
	n.Status.Allocatable = rl(cpu, mem)
	return n
}

func booking(name, site, cpu, mem string, phase brokerv1alpha1.ReservationPhase) *brokerv1alpha1.Reservation {
	return &brokerv1alpha1.Reservation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: brokerv1alpha1.ReservationSpec{
			ChunkCount: 1, Resources: rl(cpu, mem), Site: site,
		},
		Status: brokerv1alpha1.ReservationStatus{Phase: phase},
	}
}

func picker(t *testing.T, objs ...runtime.Object) *Picker {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := brokerv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return &Picker{Reader: c, Label: label, Namespace: ns}
}

func TestPickPrefersTheSiteWithMostFreeCPU(t *testing.T) {
	p := picker(t,
		node("c1", "cuneo", "4", "8Gi", true, true),
		node("a1", "asti", "8", "16Gi", true, true),
		booking("r1", "asti", "6", "4Gi", brokerv1alpha1.ReservationPhasePeered), // asti: 2 cpu left
	)
	site, err := p.Pick(context.Background(), rl("1", "1Gi"))
	if err != nil || site != "cuneo" {
		t.Fatalf("Pick = %q, %v; want cuneo", site, err)
	}
}

func TestPickNeedsTheWholeBookingInOneSite(t *testing.T) {
	p := picker(t,
		node("c1", "cuneo", "2", "4Gi", true, true),
		node("a1", "asti", "2", "4Gi", true, true),
	)
	// 3 cpu fit in the region (4 cpu) but in no single site.
	site, err := p.Pick(context.Background(), rl("3", "1Gi"))
	if err != nil || site != "" {
		t.Fatalf("Pick = %q, %v; want no site", site, err)
	}
}

func TestPickSkipsUnusableNodesAndCountsOnlyActiveBookings(t *testing.T) {
	p := picker(t,
		node("c1", "cuneo", "4", "8Gi", true, true),
		node("c2", "cuneo", "4", "8Gi", false, true), // not ready
		node("c3", "cuneo", "4", "8Gi", true, false), // cordoned
		node("x1", "", "64", "256Gi", true, true),    // no site label
		booking("gone", "cuneo", "4", "8Gi", brokerv1alpha1.ReservationPhaseReleased),
	)
	free, err := p.Free(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cuneo := free["cuneo"]
	if len(free) != 1 || cuneo.Cpu().String() != "4" {
		t.Fatalf("free = %v; want only cuneo with 4 cpu", free)
	}
	if site, _ := p.Pick(context.Background(), rl("5", "1Gi")); site != "" {
		t.Fatalf("Pick(5 cpu) = %q; want no site", site)
	}
}
