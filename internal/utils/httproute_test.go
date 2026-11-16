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

package utils

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func managedRoute(annotations map[string]string) *gatewayv1.HTTPRoute {
	all := map[string]string{ManagedByAnnotation: ManagedByValue}
	for k, v := range annotations {
		all[k] = v
	}
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app", Annotations: all},
	}
}

func TestApplyHTTPRoute_KeepsListenerGatewayAnnotation(t *testing.T) {
	tests := []struct {
		name     string
		existing map[string]string
		want     string
		wantSet  bool
	}{
		{"preserved across update", map[string]string{ListenerGatewayAnnotation: "nginx"}, "nginx", true},
		{"not invented when absent", nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := gatewayv1.Install(scheme); err != nil {
				t.Fatalf("failed to add gatewayv1 to scheme: %v", err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(managedRoute(tt.existing)).Build()
			m := &HTTPRouteManager{Client: c}

			desired := managedRoute(map[string]string{"example.com/new": "value"})
			if err := m.applyHTTPRoute(context.Background(), desired, nil); err != nil {
				t.Fatalf("applyHTTPRoute() error: %v", err)
			}

			got := &gatewayv1.HTTPRoute{}
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "api"}, got); err != nil {
				t.Fatalf("failed to get route: %v", err)
			}
			value, ok := got.Annotations[ListenerGatewayAnnotation]
			if ok != tt.wantSet || value != tt.want {
				t.Errorf("listener-gateway annotation = %q (set=%v), want %q (set=%v)", value, ok, tt.want, tt.wantSet)
			}
			if got.Annotations["example.com/new"] != "value" {
				t.Errorf("desired annotations not applied: %v", got.Annotations)
			}
			if _, ok := desired.Annotations[ListenerGatewayAnnotation]; ok {
				t.Error("applyHTTPRoute must not mutate the desired route's annotations")
			}
		})
	}
}

func sourcedRoute(name string) *gatewayv1.HTTPRoute {
	route := managedRoute(map[string]string{SourceAnnotation: "app/api"})
	route.Name = name
	return route
}

func TestApplyHTTPRoutes_UpdatesInPlaceAndDeletesObsolete(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to add gatewayv1 to scheme: %v", err)
	}
	foreign := managedRoute(map[string]string{SourceAnnotation: "app/other"})
	foreign.Name = "api-other"
	existing := sourcedRoute("api")
	existing.UID = "existing-api"
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(existing, sourcedRoute("api-2"), foreign).Build()
	m := &HTTPRouteManager{Client: c}
	ctx := context.Background()

	before := &gatewayv1.HTTPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "app", Name: "api"}, before); err != nil {
		t.Fatalf("failed to get route: %v", err)
	}
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "api"}}
	desired := []*gatewayv1.HTTPRoute{sourcedRoute("api"), sourcedRoute("api-http")}

	if err := m.ApplyHTTPRoutes(ctx, ingress, desired, nil); err != nil {
		t.Fatalf("ApplyHTTPRoutes() error: %v", err)
	}

	exists := func(name string) (*gatewayv1.HTTPRoute, bool) {
		route := &gatewayv1.HTTPRoute{}
		err := c.Get(ctx, types.NamespacedName{Namespace: "app", Name: name}, route)
		return route, err == nil
	}
	if got, ok := exists("api"); !ok || got.UID != before.UID {
		t.Errorf("route api must be updated in place, not recreated (exists=%v)", ok)
	}
	if _, ok := exists("api-http"); !ok {
		t.Error("route api-http must be created")
	}
	if _, ok := exists("api-2"); ok {
		t.Error("obsolete route api-2 must be deleted")
	}
	if _, ok := exists("api-other"); !ok {
		t.Error("route of another Ingress sharing the name prefix must be kept")
	}
}

func TestWithResolvedNamedPorts(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add corev1 to scheme: %v", err)
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "store"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "api", Port: 9000}}},
	}
	manager := &HTTPRouteManager{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(service).Build()}
	path := func(serviceName string, port networkingv1.ServiceBackendPort) networkingv1.HTTPIngressPath {
		return networkingv1.HTTPIngressPath{Path: "/", Backend: networkingv1.IngressBackend{
			Service: &networkingv1.IngressServiceBackend{Name: serviceName, Port: port},
		}}
	}
	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: "a.example.com",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{
					path("store", networkingv1.ServiceBackendPort{Name: "api"}),
					path("store", networkingv1.ServiceBackendPort{Name: "missing"}),
					path("absent", networkingv1.ServiceBackendPort{Name: "api"}),
					path("store", networkingv1.ServiceBackendPort{Number: 8080}),
				},
			}},
		}}},
	}

	resolved, unresolved := manager.WithResolvedNamedPorts(context.Background(), ingress)

	wantPorts := []networkingv1.ServiceBackendPort{{Number: 9000}, {Name: "missing"}, {Name: "api"}, {Number: 8080}}
	for i, want := range wantPorts {
		if got := resolved.Spec.Rules[0].HTTP.Paths[i].Backend.Service.Port; got != want {
			t.Errorf("path %d port = %+v, want %+v", i, got, want)
		}
	}
	if want := []string{"store:missing", "absent:api"}; strings.Join(unresolved, ",") != strings.Join(want, ",") {
		t.Errorf("unresolved = %v, want %v", unresolved, want)
	}
	if ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Name != "api" {
		t.Error("WithResolvedNamedPorts modified the original Ingress")
	}
	if !ReferencesServiceByPortName(ingress, "store") || ReferencesServiceByPortName(ingress, "other") {
		t.Error("ReferencesServiceByPortName() does not match the named-port backends")
	}
}
