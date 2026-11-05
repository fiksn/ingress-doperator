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
	"slices"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/translator"
)

func redirectIngress(annotations map[string]string, tlsHosts []string, hosts ...string) *networkingv1.Ingress {
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
		Namespace: "app", Name: "web", Annotations: annotations,
	}}
	for _, host := range hosts {
		ingress.Spec.Rules = append(ingress.Spec.Rules, networkingv1.IngressRule{Host: host})
	}
	if len(tlsHosts) > 0 {
		ingress.Spec.TLS = []networkingv1.IngressTLS{{Hosts: tlsHosts, SecretName: "web-tls"}}
	}
	return ingress
}

func identity(host string) string { return host }

func hostStrings(hosts []gatewayv1.Hostname) []string {
	out := make([]string, len(hosts))
	for i, host := range hosts {
		out[i] = string(host)
	}
	return out
}

func TestClassifyHostnames(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		tlsHosts    []string
		hosts       []string
		wantHTTPS   []string
		wantHTTP    []string
	}{
		{"TLS host is HTTPS only", nil, []string{"a.example.com"}, []string{"a.example.com"},
			[]string{"a.example.com"}, []string{}},
		{"host without TLS is HTTP only", nil, nil, []string{"a.example.com"},
			[]string{}, []string{"a.example.com"}},
		{"ssl-redirect false serves TLS host on both",
			map[string]string{nginxSSLRedirectAnnotation: "false"}, []string{"a.example.com"}, []string{"a.example.com"},
			[]string{"a.example.com"}, []string{"a.example.com"}},
		{"force-ssl-redirect keeps host without TLS off HTTP",
			map[string]string{nginxForceSSLRedirectAnnotation: "true"}, nil, []string{"a.example.com"},
			[]string{"a.example.com"}, []string{}},
		{"wildcard listener counts as certificate", nil, nil, []string{"a.review.example.com"},
			[]string{"a.review.example.com"}, []string{}},
		{"mixed Ingress", nil, []string{"a.example.com"}, []string{"a.example.com", "b.example.com"},
			[]string{"a.example.com"}, []string{"b.example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ingress := redirectIngress(tt.annotations, tt.tlsHosts, tt.hosts...)
			https, plain := classifyHostnames(ingress, identity, []WildcardListener{reviewListener})
			if got := hostStrings(https); !slices.Equal(got, tt.wantHTTPS) {
				t.Errorf("HTTPS hosts = %v, want %v", got, tt.wantHTTPS)
			}
			if got := hostStrings(plain); !slices.Equal(got, tt.wantHTTP) {
				t.Errorf("HTTP hosts = %v, want %v", got, tt.wantHTTP)
			}
		})
	}
}

func translatedRoute(hosts ...string) *gatewayv1.HTTPRoute {
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}}
	for _, host := range hosts {
		route.Spec.Hostnames = append(route.Spec.Hostnames, gatewayv1.Hostname(host))
		route.Spec.ParentRefs = append(route.Spec.ParentRefs, gatewayv1.ParentReference{
			Name:        "nginx",
			Namespace:   ptr.To(gatewayv1.Namespace(testGatewayNamespace)),
			SectionName: ptr.To(gatewayv1.SectionName(host)),
		})
	}
	return route
}

func sectionNames(route *gatewayv1.HTTPRoute) []string {
	names := make([]string, 0, len(route.Spec.ParentRefs))
	for _, ref := range route.Spec.ParentRefs {
		names = append(names, string(*ref.SectionName))
	}
	return names
}

func TestHTTPListenerRoutes(t *testing.T) {
	trans := translator.New(translator.Config{})
	mixed := redirectIngress(nil, []string{"a.example.com"}, "a.example.com", "b.example.com")

	off := &IngressReconciler{}
	if routes := off.httpListenerRoutes(mixed, trans, translatedRoute("a.example.com", "b.example.com")); len(routes) != 1 ||
		len(routes[0].Spec.Hostnames) != 2 {
		t.Errorf("without --http-redirect the route must stay as is, got %d routes", len(routes))
	}

	r := &IngressReconciler{HTTPRedirect: true}
	routes := r.httpListenerRoutes(mixed, trans, translatedRoute("a.example.com", "b.example.com"))
	if len(routes) != 2 {
		t.Fatalf("routes = %d, want HTTPS and HTTP route", len(routes))
	}
	main, plain := routes[0], routes[1]
	if main.Name != "web" || !slices.Equal(hostStrings(main.Spec.Hostnames), []string{"a.example.com"}) ||
		!slices.Equal(sectionNames(main), []string{"a.example.com"}) {
		t.Errorf("HTTPS route = %s %v %v, want web [a.example.com] on a.example.com",
			main.Name, main.Spec.Hostnames, sectionNames(main))
	}
	if plain.Name != "web-http" || !slices.Equal(hostStrings(plain.Spec.Hostnames), []string{"b.example.com"}) ||
		!slices.Equal(sectionNames(plain), []string{httpListenerName}) {
		t.Errorf("HTTP route = %s %v %v, want web-http [b.example.com] on http",
			plain.Name, plain.Spec.Hostnames, sectionNames(plain))
	}

	httpOnly := redirectIngress(nil, nil, "b.example.com")
	if routes := r.httpListenerRoutes(httpOnly, trans, translatedRoute("b.example.com")); len(routes) != 1 ||
		routes[0].Name != "web-http" {
		t.Errorf("Ingress without TLS must only get the HTTP route, got %d routes", len(routes))
	}
}

func newRedirectReconciler(t *testing.T, objs ...client.Object) *HTTPRouteReconciler {
	t.Helper()
	r := newWildcardReconciler(t, reviewListener, objs...)
	r.WildcardListeners = nil
	r.HTTPRedirect = true
	return r
}

func plainRoute(namespace, host string) gatewayv1.HTTPRoute {
	route := sourcedRoute("web-http", namespace, host)
	route.Spec.ParentRefs[0].SectionName = ptr.To(gatewayv1.SectionName(httpListenerName))
	return route
}

func TestReconcileGatewayListeners_AddsHTTPListenerAndRedirectRoute(t *testing.T) {
	gw := testGateway("nginx", true, "a.example.com")
	gw.UID = "gateway-uid"
	routes := []gatewayv1.HTTPRoute{
		sourcedRoute("web", "app", "a.example.com"),
		plainRoute("team", "b.example.com"),
	}
	r := newRedirectReconciler(t, gw,
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}},
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "web"}})

	if _, err := r.reconcileGatewayListeners(context.Background(), "nginx", routes); err != nil {
		t.Fatalf("reconcileGatewayListeners() error: %v", err)
	}

	got, _ := getGateway(t, r, "nginx")
	if names := listenerNames(got); len(names) != 2 || !names["a.example.com"] || !names[httpListenerName] {
		t.Errorf("listeners = %v, want a.example.com and http (no listener for the HTTP-only host)", names)
	}
	http := findListener(got, httpListenerName)
	if http == nil || !isHTTPListener(*http) {
		t.Fatalf("http listener = %+v, want port 80 HTTP without hostname", http)
	}
	if values := http.AllowedRoutes.Namespaces.Selector.MatchExpressions[0].Values; !slices.Equal(values,
		[]string{testGatewayNamespace, "team"}) {
		t.Errorf("http listener namespaces = %v, want [%s team]", values, testGatewayNamespace)
	}

	redirect := &gatewayv1.HTTPRoute{}
	key := types.NamespacedName{Namespace: testGatewayNamespace, Name: "nginx" + httpsRedirectRouteSuffix}
	if err := r.Get(context.Background(), key, redirect); err != nil {
		t.Fatalf("redirect HTTPRoute not created: %v", err)
	}
	filter := redirect.Spec.Rules[0].Filters[0].RequestRedirect
	if *filter.Scheme != "https" || *filter.StatusCode != 301 || len(redirect.Spec.Hostnames) != 0 {
		t.Errorf("redirect route = %+v, want catch-all 301 to https", redirect.Spec)
	}
	if redirect.OwnerReferences[0].UID != "gateway-uid" {
		t.Errorf("redirect route owner = %+v, want the Gateway", redirect.OwnerReferences)
	}
	if redirect.Annotations[ExternalDNSGatewayHostnameSource] != ExternalDNSHostnameSourceAnnotationOnly {
		t.Errorf("redirect route must be hidden from external-dns, annotations = %v", redirect.Annotations)
	}
}

func TestReconcileGatewayListeners_HTTPListenerComesBeforeNewHostnamesAtLimit(t *testing.T) {
	existing := fillerHosts("a", MaxGatewayListeners-1)
	gw := testGateway("nginx", true, existing...)
	routes := make([]gatewayv1.HTTPRoute, 0, len(existing)+1)
	for _, host := range append(existing, "new.example.com") {
		routes = append(routes, sourcedRoute(host, "app", host))
	}
	r := newRedirectReconciler(t, gw, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}})

	if _, err := r.reconcileGatewayListeners(context.Background(), "nginx", routes); err != nil {
		t.Fatalf("reconcileGatewayListeners() error: %v", err)
	}

	got, _ := getGateway(t, r, "nginx")
	names := listenerNames(got)
	if len(names) != MaxGatewayListeners || !names[httpListenerName] || names["new.example.com"] {
		t.Errorf("want the http listener to take the last slot instead of new.example.com, got %d listeners "+
			"(http=%v, new=%v)", len(names), names[httpListenerName], names["new.example.com"])
	}
}

func TestReconcileGatewayListeners_DeletesGatewayWithOnlyHTTPListener(t *testing.T) {
	gw := testGateway("nginx", true)
	gw.Spec.Listeners = []gatewayv1.Listener{{
		Name: httpListenerName, Port: httpListenerPort, Protocol: gatewayv1.HTTPProtocolType,
	}}
	redirect := desiredHTTPSRedirectRoute(gw)
	r := newRedirectReconciler(t, gw, redirect)

	routes, err := r.listHTTPRoutesForGateway(context.Background(), "nginx", "")
	if err != nil {
		t.Fatalf("listHTTPRoutesForGateway() error: %v", err)
	}
	if len(routes) != 0 {
		t.Fatalf("redirect route must not count as a route of the Gateway, got %d", len(routes))
	}
	if _, err := r.reconcileGatewayListeners(context.Background(), "nginx", routes); err != nil {
		t.Fatalf("reconcileGatewayListeners() error: %v", err)
	}
	if _, ok := getGateway(t, r, "nginx"); ok {
		t.Error("Gateway left with only the http listener and no routes must be deleted")
	}
}

func TestUpdateGatewayListeners_PlainHTTPRouteOnlyJoinsHTTPListener(t *testing.T) {
	gw := testGateway("nginx", true, "a.example.com")
	route := plainRoute("team", "b.example.com")
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "web"}}
	r := &HTTPRouteReconciler{GatewayNamespace: testGatewayNamespace, HTTPRedirect: true}

	updated, _ := r.updateGatewayListeners(context.Background(), gw, &route, ingress)

	names := listenerNames(gw)
	if !updated || len(names) != 2 || !names[httpListenerName] || names["b.example.com"] {
		t.Errorf("listeners = %v, want http added and no listener for b.example.com", names)
	}
	values := findListener(gw, httpListenerName).AllowedRoutes.Namespaces.Selector.MatchExpressions[0].Values
	if !slices.Contains(values, "team") {
		t.Errorf("http listener namespaces = %v, want team allowed", values)
	}
}

func TestSyncHTTPSRedirectRoute_LeavesForeignRoute(t *testing.T) {
	gw := testGateway("nginx", true)
	gw.UID = "gateway-uid"
	gw.Spec.Listeners = []gatewayv1.Listener{{
		Name: httpListenerName, Port: httpListenerPort, Protocol: gatewayv1.HTTPProtocolType,
	}}
	foreign := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace: testGatewayNamespace, Name: "nginx" + httpsRedirectRouteSuffix,
		Annotations: map[string]string{"owner": "me"},
	}}
	r := newRedirectReconciler(t, gw, foreign)

	if err := r.syncHTTPSRedirectRoute(context.Background(), gw); err != nil {
		t.Fatalf("syncHTTPSRedirectRoute() error: %v", err)
	}
	got := &gatewayv1.HTTPRoute{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(foreign), got); err != nil {
		t.Fatalf("failed to get route: %v", err)
	}
	if got.Annotations["owner"] != "me" || len(got.Spec.Rules) != 0 {
		t.Errorf("foreign route must be left untouched, got %+v", got)
	}
}
