/*
Copyright Gregor Pogacnik 2026.

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

package controller

import (
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/translator"
)

var (
	testDefaultGatewayAnnotations = map[string]string{
		"cert-manager.io/acme-challenge-type": "dns01",
		"cert-manager.io/cluster-issuer":      "letsencrypt-cert-manager",
	}
	testInfrastructureAnnotations = map[string]string{
		"service.beta.kubernetes.io/aws-load-balancer-type": "nlb",
	}
)

func assertConfiguredGatewayAnnotations(t *testing.T, gw *gatewayv1.Gateway) {
	t.Helper()
	for key, want := range testDefaultGatewayAnnotations {
		if got := gw.Annotations[key]; got != want {
			t.Errorf("annotation %q = %q, want %q", key, got, want)
		}
	}
	for key, want := range testInfrastructureAnnotations {
		if gw.Spec.Infrastructure == nil {
			t.Fatal("spec.infrastructure is nil, want infrastructure annotations")
		}
		got := gw.Spec.Infrastructure.Annotations[gatewayv1.AnnotationKey(key)]
		if string(got) != want {
			t.Errorf("infrastructure annotation %q = %q, want %q", key, got, want)
		}
	}
}

func TestCreateInitialGateway_SetsConfiguredAnnotations(t *testing.T) {
	r := &HTTPRouteReconciler{
		GatewayNamespace:                 testGatewayNamespace,
		DefaultGatewayAnnotations:        testDefaultGatewayAnnotations,
		GatewayInfrastructureAnnotations: testInfrastructureAnnotations,
	}

	gw := r.createInitialGateway("nginx", nil)

	assertConfiguredGatewayAnnotations(t, gw)
	if gw.Annotations[translator.ManagedByAnnotation] != translator.ManagedByValue {
		t.Error("initial Gateway must keep the managed-by annotation")
	}
}

func TestCreateInitialGateway_NoConfiguredAnnotations(t *testing.T) {
	gw := (&HTTPRouteReconciler{}).createInitialGateway("nginx", nil)

	if gw.Spec.Infrastructure != nil {
		t.Errorf("spec.infrastructure = %+v, want nil without infrastructure annotations", gw.Spec.Infrastructure)
	}
	if len(gw.Annotations) != 1 {
		t.Errorf("annotations = %v, want only managed-by", gw.Annotations)
	}
}

func TestIngressReconciler_NewGatewayCarriesConfiguredAnnotations(t *testing.T) {
	r := &IngressReconciler{
		Client:                           newReleaseReconciler(t).Client,
		GatewayNamespace:                 testGatewayNamespace,
		DefaultGatewayAnnotations:        testDefaultGatewayAnnotations,
		GatewayInfrastructureAnnotations: testInfrastructureAnnotations,
	}

	gw, canManage, exists, err := r.ensureGatewayForListenerUpdate(context.Background(), "nginx", nil)
	if err != nil {
		t.Fatalf("ensureGatewayForListenerUpdate() error: %v", err)
	}
	if !canManage || exists {
		t.Fatalf("canManage=%v exists=%v, want a new manageable Gateway", canManage, exists)
	}
	assertConfiguredGatewayAnnotations(t, gw)
}

func TestReconcileGatewayListeners_AddsConfiguredAnnotationsToExistingGateway(t *testing.T) {
	gw := testGateway("nginx", true, "web.example.com")
	gw.Annotations["cert-manager.io/cluster-issuer"] = "old-issuer"
	gw.Annotations["example.com/unrelated"] = "keep"
	route := testRoute("web", "nginx", "nginx", "web.example.com")
	r := newReleaseReconciler(t, gw, route)
	r.DefaultGatewayAnnotations = testDefaultGatewayAnnotations
	r.GatewayInfrastructureAnnotations = testInfrastructureAnnotations

	updated, err := r.reconcileGatewayListeners(context.Background(), "nginx", []gatewayv1.HTTPRoute{*route})
	if err != nil {
		t.Fatalf("reconcileGatewayListeners() error: %v", err)
	}
	if !updated {
		t.Error("reconcileGatewayListeners() = false, want true when annotations are missing")
	}

	got, _ := getGateway(t, r, "nginx")
	assertConfiguredGatewayAnnotations(t, got)
	if got.Annotations["example.com/unrelated"] != "keep" {
		t.Error("annotations not set by the flags must be left untouched")
	}
}

func TestApplyConfiguredGatewayAnnotations_ReportsNoChangeWhenUpToDate(t *testing.T) {
	r := &HTTPRouteReconciler{
		DefaultGatewayAnnotations:        testDefaultGatewayAnnotations,
		GatewayInfrastructureAnnotations: testInfrastructureAnnotations,
	}
	gw := testGateway("nginx", true)

	if !r.applyConfiguredGatewayAnnotations(gw, nil) {
		t.Error("first apply = false, want true")
	}
	if r.applyConfiguredGatewayAnnotations(gw, nil) {
		t.Error("second apply = true, want false so up-to-date Gateways are not rewritten")
	}
}

var testAnnotationsByClass = []translator.IngressClassAnnotationsRule{
	{Pattern: "*private*", Annotations: map[string]string{"lb-scheme": "internal"}},
	{Pattern: "*public*", Annotations: map[string]string{"lb-scheme": "internet-facing"}},
	{Pattern: "*", Annotations: map[string]string{"lb-type": "nlb"}},
}

func classIngress(class string) *networkingv1.Ingress {
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}}
	if class != "" {
		ingress.Spec.IngressClassName = ptr.To(class)
	}
	return ingress
}

func infraAnnotations(gw *gatewayv1.Gateway) map[string]string {
	out := make(map[string]string)
	if gw.Spec.Infrastructure == nil {
		return out
	}
	for key, value := range gw.Spec.Infrastructure.Annotations {
		out[string(key)] = string(value)
	}
	return out
}

func TestCreateInitialGateway_AppliesAnnotationsByClass(t *testing.T) {
	tests := []struct {
		class string
		want  map[string]string
	}{
		{"nginx-private", map[string]string{"lb-scheme": "internal", "lb-type": "nlb", "global": "yes"}},
		{"nginx-public", map[string]string{"lb-scheme": "internet-facing", "lb-type": "nlb", "global": "yes"}},
		{"", map[string]string{"lb-type": "nlb", "global": "yes"}},
	}
	for _, tt := range tests {
		t.Run(tt.class, func(t *testing.T) {
			r := &HTTPRouteReconciler{
				GatewayInfrastructureAnnotations: map[string]string{"global": "yes", "lb-type": "overridden"},
				InfrastructureAnnotationsByClass: testAnnotationsByClass,
				IngressClassEmpty:                "none",
			}
			got := infraAnnotations(r.createInitialGateway("gw", classIngress(tt.class)))
			if len(got) != len(tt.want) {
				t.Errorf("infrastructure annotations = %v, want %v", got, tt.want)
			}
			for key, want := range tt.want {
				if got[key] != want {
					t.Errorf("infrastructure annotation %q = %q, want %q", key, got[key], want)
				}
			}
		})
	}
}

func TestReconcileGatewayListeners_AppliesAnnotationsByClassFromSourceIngress(t *testing.T) {
	gw := testGateway("nginx-private", true, "web.example.com")
	ingress := classIngress("nginx-private")
	ingress.Spec.Rules = []networkingv1.IngressRule{{Host: "web.example.com"}}
	route := testRoute("web", "nginx-private", "nginx-private", "web.example.com")
	route.Annotations[translator.SourceAnnotation] = "app/web"
	r := newReleaseReconciler(t, gw, route, ingress)
	r.InfrastructureAnnotationsByClass = testAnnotationsByClass

	routes := []gatewayv1.HTTPRoute{*route}
	if _, err := r.reconcileGatewayListeners(context.Background(), "nginx-private", routes); err != nil {
		t.Fatalf("reconcileGatewayListeners() error: %v", err)
	}

	got, _ := getGateway(t, r, "nginx-private")
	infra := infraAnnotations(got)
	if infra["lb-scheme"] != "internal" || infra["lb-type"] != "nlb" {
		t.Errorf("infrastructure annotations = %v, want private class rules applied", infra)
	}
}

func TestReconcileGatewayListeners_KeepsClassAnnotationsWithoutSourceIngress(t *testing.T) {
	gw := testGateway("nginx", true, "web.example.com")
	gw.Spec.Infrastructure = &gatewayv1.GatewayInfrastructure{
		Annotations: map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue{"lb-scheme": "internal"},
	}
	route := testRoute("web", "nginx", "nginx", "web.example.com")
	r := newReleaseReconciler(t, gw, route)
	r.InfrastructureAnnotationsByClass = testAnnotationsByClass

	if _, err := r.reconcileGatewayListeners(context.Background(), "nginx", []gatewayv1.HTTPRoute{*route}); err != nil {
		t.Fatalf("reconcileGatewayListeners() error: %v", err)
	}

	got, _ := getGateway(t, r, "nginx")
	if infra := infraAnnotations(got); len(infra) != 1 || infra["lb-scheme"] != "internal" {
		t.Errorf("infrastructure annotations = %v, want existing values untouched", infra)
	}
}
