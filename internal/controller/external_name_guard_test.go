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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/translator"
	"github.com/fiksn/ingress-doperator/internal/utils"
)

func externalNameService(name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: multiHostNamespace, Name: name},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "registry.example.com"},
	}
}

func clusterIPService(name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: multiHostNamespace, Name: name},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 80}}},
	}
}

func routesOf(t *testing.T, r *IngressReconciler, ingress *networkingv1.Ingress) []gatewayv1.HTTPRoute {
	t.Helper()
	list := &gatewayv1.HTTPRouteList{}
	if err := r.List(context.Background(), list, client.InNamespace(ingress.Namespace)); err != nil {
		t.Fatalf("failed to list HTTPRoutes: %v", err)
	}
	var routes []gatewayv1.HTTPRoute
	for _, route := range list.Items {
		if utils.IsManagedByUsForIngress(&route, ingress.Namespace, ingress.Name) {
			routes = append(routes, route)
		}
	}
	return routes
}

func TestExternalNameBackend_SkippedByDefault(t *testing.T) {
	ingress := multiHostIngress(hostRule(consoleHost, ingressPath("/", networkingv1.PathTypePrefix, "registry", numberedPort(443))))
	// A route generated before the guard existed must go, so it no longer breaks the Gateway.
	stale := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace: ingress.Namespace, Name: ingress.Name,
		Annotations: map[string]string{
			translator.ManagedByAnnotation: translator.ManagedByValue,
			translator.SourceAnnotation:    ingress.Namespace + "/" + ingress.Name,
		},
	}}
	recorder := events.NewFakeRecorder(10)
	r := newTranslationReconciler(t, ingress, externalNameService("registry"), stale)
	r.Recorder = recorder

	result := reconcileIngress(t, r, ingress)

	if result.RequeueAfter != externalNameRetryInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, externalNameRetryInterval)
	}
	if routes := routesOf(t, r, ingress); len(routes) != 0 {
		t.Errorf("got %d HTTPRoutes for an Ingress with an ExternalName backend, want none", len(routes))
	}
	got := drainEvents(recorder)
	if !strings.Contains(got, "Warning ExternalNameBackendUnsupported") || !strings.Contains(got, "registry") ||
		!strings.Contains(got, "--external-name-services=allow") {
		t.Errorf("no ExternalNameBackendUnsupported warning naming the Service and the flag:\n%s", got)
	}
}

func TestExternalNameBackend_TranslatedWhenAllowed(t *testing.T) {
	ingress := multiHostIngress(hostRule(consoleHost, ingressPath("/", networkingv1.PathTypePrefix, "registry", numberedPort(443))))

	routes := translateIngressWith(t, func(r *IngressReconciler) { r.AllowExternalNameServices = true },
		ingress, externalNameService("registry"))

	assertRoutedTo(t, routes, consoleHost, "/", backend{"registry", 443})
}

func TestExternalNameBackend_OtherServicesUnaffected(t *testing.T) {
	ingress := multiHostIngress(hostRule(consoleHost, ingressPath("/", networkingv1.PathTypePrefix, "web", numberedPort(80))))

	routes := translateIngress(t, ingress, clusterIPService("web"))

	assertRoutedTo(t, routes, consoleHost, "/", backend{"web", 80})
}

func TestExternalNameBackend_InCanarySibling(t *testing.T) {
	primary := multiHostIngress(hostRule(consoleHost, ingressPath("/", networkingv1.PathTypePrefix, "web", numberedPort(80))))
	canary := multiHostIngress(hostRule(consoleHost, ingressPath("/", networkingv1.PathTypePrefix, "registry", numberedPort(443))))
	canary.Name, canary.UID = "objects-canary", "objects-canary-uid"
	canary.Annotations = map[string]string{translator.CanaryAnnotation: "true", translator.CanaryWeightAnnotation: "10"}
	r := newTranslationReconciler(t, primary, canary, clusterIPService("web"), externalNameService("registry"))

	result := reconcileIngress(t, r, primary)

	if result.RequeueAfter != externalNameRetryInterval || len(routesOf(t, r, primary)) != 0 {
		t.Errorf("primary with an ExternalName canary backend was translated (result %+v)", result)
	}
}

// TestBuildAuthInputs_DNSResolverFollowsExternalNameServices checks that
// --external-name-services=allow, which asserts a DNS resolver in NGINX Gateway Fabric, also
// enables snippets that need one (mirror-target).
func TestBuildAuthInputs_DNSResolverFollowsExternalNameServices(t *testing.T) {
	ingress := multiHostIngress(hostRule(consoleHost, ingressPath("/", networkingv1.PathTypePrefix, "web", numberedPort(80))))
	for _, allow := range []bool{false, true} {
		r := newTranslationReconciler(t, ingress)
		r.AllowExternalNameServices = allow
		if got := r.buildAuthInputs(context.Background(), ingress).DNSResolver; got != allow {
			t.Errorf("AllowExternalNameServices=%v: DNSResolver = %v", allow, got)
		}
	}
}
