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
	"fmt"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/fiksn/ingress-doperator/internal/utils"
)

// These tests translate Ingresses whose rules route different hosts to different backends and
// check, for a request to a host and path, which Service and port the generated HTTPRoutes send it
// to. Matching follows Gateway API precedence (see routedBackend), so the tests hold however the
// translation scopes rules to hosts (one route per host, or otherwise).

const (
	multiHostNamespace = "storage"
	consoleHost        = "console.example.com"
	apiHost            = "api.example.com"
	consolePortNumber  = 9001
	apiPortNumber      = 9000
)

type backend struct {
	service string
	port    int32
}

func (b backend) String() string { return fmt.Sprintf("%s:%d", b.service, b.port) }

func ingressPath(path string, pathType networkingv1.PathType, service string, port networkingv1.ServiceBackendPort) networkingv1.HTTPIngressPath {
	return networkingv1.HTTPIngressPath{
		Path:     path,
		PathType: ptr.To(pathType),
		Backend: networkingv1.IngressBackend{
			Service: &networkingv1.IngressServiceBackend{Name: service, Port: port},
		},
	}
}

func hostRule(host string, paths ...networkingv1.HTTPIngressPath) networkingv1.IngressRule {
	return networkingv1.IngressRule{
		Host:             host,
		IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: paths}},
	}
}

func namedPort(name string) networkingv1.ServiceBackendPort {
	return networkingv1.ServiceBackendPort{Name: name}
}

func numberedPort(number int32) networkingv1.ServiceBackendPort {
	return networkingv1.ServiceBackendPort{Number: number}
}

func multiHostIngress(rules ...networkingv1.IngressRule) *networkingv1.Ingress {
	hosts := make([]string, 0, len(rules))
	for _, rule := range rules {
		hosts = append(hosts, rule.Host)
	}
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: multiHostNamespace, Name: "objects", UID: "objects-uid"},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ptr.To("private"),
			Rules:            rules,
			TLS:              []networkingv1.IngressTLS{{Hosts: hosts, SecretName: "objects-tls"}},
		},
	}
}

// storeService has two named ports, like a server with a console and an API port.
func storeService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: multiHostNamespace, Name: "store"},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Name: "console", Port: consolePortNumber, TargetPort: intstr.FromInt32(consolePortNumber)},
			{Name: "api", Port: apiPortNumber, TargetPort: intstr.FromInt32(apiPortNumber)},
		}},
	}
}

// translateIngress runs the Ingress reconcile against a fake cluster holding objs and returns the
// HTTPRoutes generated for the Ingress.
func translateIngress(t *testing.T, ingress *networkingv1.Ingress, objs ...client.Object) []gatewayv1.HTTPRoute {
	t.Helper()
	return translateIngressWith(t, func(*IngressReconciler) {}, ingress, objs...)
}

// translateIngressWith is translateIngress with configure applied to the reconciler first.
func translateIngressWith(
	t *testing.T,
	configure func(*IngressReconciler),
	ingress *networkingv1.Ingress,
	objs ...client.Object,
) []gatewayv1.HTTPRoute {
	t.Helper()
	r := newTranslationReconciler(t, append([]client.Object{ingress}, objs...)...)
	configure(r)
	reconcileIngress(t, r, ingress)
	return ingressRoutes(t, r, ingress)
}

// newTranslationReconciler returns an IngressReconciler on a fake cluster holding objs.
func newTranslationReconciler(t *testing.T, objs ...client.Object) *IngressReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, networkingv1.AddToScheme, gatewayv1.Install, gatewayv1beta1.Install,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("failed to build scheme: %v", err)
		}
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&networkingv1.Ingress{}).Build()
	return &IngressReconciler{
		Client:                    cli,
		Scheme:                    scheme,
		GatewayNamespace:          testGatewayNamespace,
		GatewayName:               "shared",
		GatewayClassName:          "nginx",
		IngressPostProcessingMode: IngressPostProcessingModeNone,
		IngressClassFilters:       []string{"*"},
		HTTPRouteManager:          &utils.HTTPRouteManager{Client: cli},
	}
}

func reconcileIngress(t *testing.T, r *IngressReconciler, ingress *networkingv1.Ingress) ctrl.Result {
	t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ingress.Namespace, Name: ingress.Name}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}
	return result
}

// ingressRoutes returns the HTTPRoutes generated for the Ingress.
func ingressRoutes(t *testing.T, r *IngressReconciler, ingress *networkingv1.Ingress) []gatewayv1.HTTPRoute {
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
	if len(routes) == 0 {
		t.Fatal("Reconcile() generated no HTTPRoute")
	}
	return routes
}

// routedBackend returns the backend Gateway API routes a request for host and path to: among the
// rules of routes whose hostnames include host, an Exact path match beats a PathPrefix match, a
// longer prefix beats a shorter one, and remaining ties go to the route that sorts first by
// namespace/name, then to the earlier rule. Matches with conditions other than the path (headers,
// query parameters, method) do not match, as the request carries none.
func routedBackend(routes []gatewayv1.HTTPRoute, host, path string) (backend, bool) {
	sorted := append([]gatewayv1.HTTPRoute(nil), routes...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Namespace+"/"+sorted[i].Name < sorted[j].Namespace+"/"+sorted[j].Name
	})
	var best *gatewayv1.HTTPRouteRule
	bestExact, bestLength := false, -1
	for i := range sorted {
		if !routeServesHost(&sorted[i], host) {
			continue
		}
		for j := range sorted[i].Spec.Rules {
			rule := &sorted[i].Spec.Rules[j]
			exact, length, ok := ruleMatch(rule, path)
			if !ok {
				continue
			}
			if best == nil || (exact && !bestExact) || (exact == bestExact && length > bestLength) {
				best, bestExact, bestLength = rule, exact, length
			}
		}
	}
	if best == nil || len(best.BackendRefs) == 0 {
		return backend{}, false
	}
	ref := best.BackendRefs[0]
	port := int32(0)
	if ref.Port != nil {
		port = *ref.Port
	}
	return backend{service: string(ref.Name), port: port}, true
}

func routeServesHost(route *gatewayv1.HTTPRoute, host string) bool {
	for _, hostname := range route.Spec.Hostnames {
		if string(hostname) == host {
			return true
		}
	}
	return false
}

// ruleMatch reports whether rule matches a request for path, whether by an Exact path match, and
// the length of the matched path value.
func ruleMatch(rule *gatewayv1.HTTPRouteRule, path string) (exact bool, length int, ok bool) {
	if len(rule.Matches) == 0 {
		return false, 1, true
	}
	ok = false
	for _, match := range rule.Matches {
		if len(match.Headers) > 0 || len(match.QueryParams) > 0 || match.Method != nil {
			continue
		}
		matchType, value := gatewayv1.PathMatchPathPrefix, "/"
		if match.Path != nil {
			if match.Path.Type != nil {
				matchType = *match.Path.Type
			}
			if match.Path.Value != nil {
				value = *match.Path.Value
			}
		}
		matchExact, matched := pathMatches(matchType, value, path)
		if !matched {
			continue
		}
		if !ok || (matchExact && !exact) || (matchExact == exact && len(value) > length) {
			exact, length, ok = matchExact, len(value), true
		}
	}
	return exact, length, ok
}

func pathMatches(matchType gatewayv1.PathMatchType, value, path string) (exact bool, matched bool) {
	switch matchType {
	case gatewayv1.PathMatchExact:
		return true, path == value
	case gatewayv1.PathMatchPathPrefix:
		prefix := strings.TrimSuffix(value, "/")
		return false, prefix == "" || path == prefix || strings.HasPrefix(path, prefix+"/")
	default:
		return false, false
	}
}

func assertRoutedTo(t *testing.T, routes []gatewayv1.HTTPRoute, host, path string, want backend) {
	t.Helper()
	got, ok := routedBackend(routes, host, path)
	if !ok {
		t.Errorf("request to %s%s is not routed anywhere; want %s", host, path, want)
		return
	}
	if got != want {
		t.Errorf("request to %s%s is routed to %s, want %s", host, path, got, want)
	}
}

// TestMultiHostIngress_SameServiceNamedPortsPerHost is an Ingress with two hosts on the same path,
// served by the same Service on different named ports. Each host must reach its own port.
func TestMultiHostIngress_SameServiceNamedPortsPerHost(t *testing.T) {
	ingress := multiHostIngress(
		hostRule(consoleHost, ingressPath("/", networkingv1.PathTypeImplementationSpecific, "store", namedPort("console"))),
		hostRule(apiHost, ingressPath("/", networkingv1.PathTypeImplementationSpecific, "store", namedPort("api"))),
	)

	routes := translateIngress(t, ingress, storeService())

	for _, path := range []string{"/", "/bucket/object"} {
		assertRoutedTo(t, routes, consoleHost, path, backend{"store", consolePortNumber})
		assertRoutedTo(t, routes, apiHost, path, backend{"store", apiPortNumber})
	}
}

// TestMultiHostIngress_DifferentServicesPerHost isolates host scoping: two hosts on the same path,
// each served by its own Service on a numeric port.
func TestMultiHostIngress_DifferentServicesPerHost(t *testing.T) {
	ingress := multiHostIngress(
		hostRule(consoleHost, ingressPath("/", networkingv1.PathTypePrefix, "console", numberedPort(80))),
		hostRule(apiHost, ingressPath("/", networkingv1.PathTypePrefix, "api", numberedPort(8080))),
	)

	routes := translateIngress(t, ingress)

	assertRoutedTo(t, routes, consoleHost, "/", backend{"console", 80})
	assertRoutedTo(t, routes, apiHost, "/", backend{"api", 8080})
}

// TestMultiHostIngress_HostOnlyPathDoesNotLeak checks that a more specific path defined for one
// host does not capture requests to another host.
func TestMultiHostIngress_HostOnlyPathDoesNotLeak(t *testing.T) {
	ingress := multiHostIngress(
		hostRule(consoleHost,
			ingressPath("/", networkingv1.PathTypePrefix, "console", numberedPort(80)),
			ingressPath("/admin", networkingv1.PathTypePrefix, "admin", numberedPort(80)),
		),
		hostRule(apiHost, ingressPath("/", networkingv1.PathTypePrefix, "api", numberedPort(8080))),
	)

	routes := translateIngress(t, ingress)

	assertRoutedTo(t, routes, consoleHost, "/admin", backend{"admin", 80})
	assertRoutedTo(t, routes, apiHost, "/admin", backend{"api", 8080})
}

// TestSingleHostIngress_SameServiceNamedPortsPerPath isolates named-port resolution: one host,
// two paths served by the same Service on different named ports.
func TestSingleHostIngress_SameServiceNamedPortsPerPath(t *testing.T) {
	ingress := multiHostIngress(hostRule(consoleHost,
		ingressPath("/ui", networkingv1.PathTypePrefix, "store", namedPort("console")),
		ingressPath("/s3", networkingv1.PathTypePrefix, "store", namedPort("api")),
	))

	routes := translateIngress(t, ingress, storeService())

	assertRoutedTo(t, routes, consoleHost, "/ui", backend{"store", consolePortNumber})
	assertRoutedTo(t, routes, consoleHost, "/s3", backend{"store", apiPortNumber})
}

// TestMultiHostIngress_SharedPathsStillWork is the case translation already handles: two hosts
// with identical rules must both reach the shared backend.
func TestMultiHostIngress_SharedPathsStillWork(t *testing.T) {
	ingress := multiHostIngress(
		hostRule(consoleHost, ingressPath("/", networkingv1.PathTypePrefix, "web", numberedPort(80))),
		hostRule(apiHost, ingressPath("/", networkingv1.PathTypePrefix, "web", numberedPort(80))),
	)

	routes := translateIngress(t, ingress)

	assertRoutedTo(t, routes, consoleHost, "/", backend{"web", 80})
	assertRoutedTo(t, routes, apiHost, "/", backend{"web", 80})
}

// TestRoutedBackend checks the matching the tests above rely on, against hand-written routes.
func TestRoutedBackend(t *testing.T) {
	rule := func(matchType gatewayv1.PathMatchType, path, service string, headers ...gatewayv1.HTTPHeaderMatch) gatewayv1.HTTPRouteRule {
		return gatewayv1.HTTPRouteRule{
			Matches: []gatewayv1.HTTPRouteMatch{{
				Path:    &gatewayv1.HTTPPathMatch{Type: ptr.To(matchType), Value: ptr.To(path)},
				Headers: headers,
			}},
			BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: gatewayv1.ObjectName(service), Port: ptr.To(gatewayv1.PortNumber(80)),
				},
			}}},
		}
	}
	route := func(name, host string, rules ...gatewayv1.HTTPRouteRule) gatewayv1.HTTPRoute {
		return gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Namespace: multiHostNamespace, Name: name},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(host)},
				Rules:     rules,
			},
		}
	}
	prefix, exact := gatewayv1.PathMatchPathPrefix, gatewayv1.PathMatchExact
	hostHeader := gatewayv1.HTTPHeaderMatch{Name: "X-Env", Value: "canary"}
	routes := []gatewayv1.HTTPRoute{
		route("b-console", consoleHost,
			rule(prefix, "/", "console"),
			rule(prefix, "/admin", "admin"),
			rule(exact, "/admin/login", "login"),
			rule(prefix, "/admin", "shadowed-by-earlier-rule"),
			rule(prefix, "/canary", "canary", hostHeader),
		),
		route("a-api", apiHost, rule(prefix, "/", "api")),
		route("c-api-later", apiHost, rule(prefix, "/", "later-route")),
	}
	tests := []struct {
		host, path string
		want       string
	}{
		{consoleHost, "/", "console"},
		{consoleHost, "/console", "console"},
		{consoleHost, "/admin", "admin"},
		{consoleHost, "/admin/users", "admin"},
		{consoleHost, "/administrator", "console"},
		{consoleHost, "/admin/login", "login"},
		{consoleHost, "/canary", "console"},
		{apiHost, "/admin", "api"},
		{"other.example.com", "/", ""},
	}
	for _, tt := range tests {
		got, ok := routedBackend(routes, tt.host, tt.path)
		switch {
		case tt.want == "" && ok:
			t.Errorf("routedBackend(%s%s) = %s, want no match", tt.host, tt.path, got)
		case tt.want != "" && got.service != tt.want:
			t.Errorf("routedBackend(%s%s) = %s, want %s", tt.host, tt.path, got.service, tt.want)
		}
	}
}

// TestMultiHostIngress_WithHTTPRedirect checks that the --http-redirect split keeps every route
// on its own hosts: TLS hosts stay on their HTTPS routes, the plain HTTP host gets its own route.
func TestMultiHostIngress_WithHTTPRedirect(t *testing.T) {
	const plainHost = "plain.example.com"
	ingress := multiHostIngress(
		hostRule(consoleHost, ingressPath("/", networkingv1.PathTypeImplementationSpecific, "store", namedPort("console"))),
		hostRule(apiHost, ingressPath("/", networkingv1.PathTypeImplementationSpecific, "store", namedPort("api"))),
		hostRule(plainHost, ingressPath("/", networkingv1.PathTypePrefix, "plain", numberedPort(8080))),
	)
	ingress.Spec.TLS = []networkingv1.IngressTLS{{Hosts: []string{consoleHost, apiHost}, SecretName: "objects-tls"}}

	routes := translateIngressWith(t, func(r *IngressReconciler) { r.HTTPRedirect = true }, ingress, storeService())

	assertRoutedTo(t, routes, consoleHost, "/", backend{"store", consolePortNumber})
	assertRoutedTo(t, routes, apiHost, "/", backend{"store", apiPortNumber})
	assertRoutedTo(t, routes, plainHost, "/", backend{"plain", 8080})
	for _, route := range routes {
		if len(route.Spec.Hostnames) == 0 {
			t.Errorf("HTTPRoute %s has no hostnames", route.Name)
		}
		for _, hostname := range route.Spec.Hostnames {
			if attachesToHTTPListener(&route) != (hostname == plainHost) {
				t.Errorf("HTTPRoute %s serves %s on the wrong listener", route.Name, hostname)
			}
		}
	}
}

// TestMultiHostIngress_KeepsRouteNames checks that an Ingress whose hosts share their paths keeps
// its single route, and that splitting by host keeps the Ingress's name for the first host's
// route, so existing routes are updated rather than recreated.
func TestMultiHostIngress_KeepsRouteNames(t *testing.T) {
	shared := multiHostIngress(
		hostRule(consoleHost, ingressPath("/", networkingv1.PathTypePrefix, "web", numberedPort(80))),
		hostRule(apiHost, ingressPath("/", networkingv1.PathTypePrefix, "web", numberedPort(80))),
	)
	split := multiHostIngress(
		hostRule(consoleHost, ingressPath("/", networkingv1.PathTypePrefix, "console", numberedPort(80))),
		hostRule(apiHost, ingressPath("/", networkingv1.PathTypePrefix, "api", numberedPort(8080))),
	)
	tests := []struct {
		name    string
		ingress *networkingv1.Ingress
		want    []string
	}{
		{"shared paths", shared, []string{"objects"}},
		{"different paths", split, []string{"objects", "objects-" + apiHost}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, route := range translateIngress(t, tt.ingress) {
				got = append(got, route.Name)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("HTTPRoute names = %v, want %v", got, tt.want)
			}
		})
	}
}

func minioShapedIngress() *networkingv1.Ingress {
	return multiHostIngress(
		hostRule(consoleHost, ingressPath("/", networkingv1.PathTypeImplementationSpecific, "store", namedPort("console"))),
		hostRule(apiHost, ingressPath("/", networkingv1.PathTypeImplementationSpecific, "store", namedPort("api"))),
	)
}

// TestNamedPorts_ServiceCreatedAfterIngress covers the Service appearing after the Ingress was
// translated: the reconcile requeues instead of settling on the fallback port, and creating the
// Service re-queues the Ingress even though the reconcile cache has seen it.
func TestNamedPorts_ServiceCreatedAfterIngress(t *testing.T) {
	ingress := minioShapedIngress()
	r := newTranslationReconciler(t, ingress)
	r.ReconcileCache = map[string]utils.ReconcileCacheEntry{}

	result := reconcileIngress(t, r, ingress)
	if result.RequeueAfter == 0 {
		t.Error("Reconcile() with unresolved named ports did not requeue")
	}
	if len(r.ReconcileCache) != 0 {
		t.Errorf("Ingress with unresolved named ports was recorded in the reconcile cache: %v", r.ReconcileCache)
	}

	service := storeService()
	if err := r.Create(context.Background(), service); err != nil {
		t.Fatalf("failed to create Service: %v", err)
	}
	if requests := r.enqueueIngressesForService(context.Background(), service); len(requests) != 1 ||
		requests[0].Name != ingress.Name {
		t.Fatalf("enqueueIngressesForService() = %v, want the Ingress", requests)
	}

	if result := reconcileIngress(t, r, ingress); result.RequeueAfter != 0 {
		t.Errorf("Reconcile() requeued after the Service was created: %+v", result)
	}
	routes := ingressRoutes(t, r, ingress)
	assertRoutedTo(t, routes, consoleHost, "/", backend{"store", consolePortNumber})
	assertRoutedTo(t, routes, apiHost, "/", backend{"store", apiPortNumber})
}

// TestNamedPorts_ServicePortChanges covers a Service whose port numbers change after the Ingress
// was reconciled and recorded in the reconcile cache.
func TestNamedPorts_ServicePortChanges(t *testing.T) {
	ingress := minioShapedIngress()
	service := storeService()
	r := newTranslationReconciler(t, ingress, service)
	r.ReconcileCache = map[string]utils.ReconcileCacheEntry{}
	reconcileIngress(t, r, ingress)

	service.Spec.Ports[1].Port = 9100
	if err := r.Update(context.Background(), service); err != nil {
		t.Fatalf("failed to update Service: %v", err)
	}
	r.enqueueIngressesForService(context.Background(), service)
	reconcileIngress(t, r, ingress)

	assertRoutedTo(t, ingressRoutes(t, r, ingress), apiHost, "/", backend{"store", 9100})
}

func TestEnqueueIngressesForService_OnlyNamedPortUsers(t *testing.T) {
	named := minioShapedIngress()
	numbered := multiHostIngress(hostRule("other.example.com",
		ingressPath("/", networkingv1.PathTypePrefix, "store", numberedPort(9000))))
	numbered.Name, numbered.UID = "numbered", "numbered-uid"
	r := newTranslationReconciler(t, named, numbered)

	requests := r.enqueueIngressesForService(context.Background(), storeService())

	if len(requests) != 1 || requests[0].Name != named.Name {
		t.Errorf("enqueueIngressesForService() = %v, want only %s", requests, named.Name)
	}
}
