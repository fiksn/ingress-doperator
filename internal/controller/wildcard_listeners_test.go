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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/fiksn/ingress-doperator/internal/translator"
)

const reviewWildcard = "*.review.example.com"

var reviewListener = WildcardListener{
	Hostname: reviewWildcard, SecretNamespace: testGatewayNamespace, SecretName: "review-wildcard-tls",
}

func TestParseWildcardListeners(t *testing.T) {
	got, err := ParseWildcardListeners(
		"*.review.example.com=gateways/review-tls, *.dev.example.com=cert-manager/dev-tls")
	if err != nil {
		t.Fatalf("ParseWildcardListeners() error: %v", err)
	}
	want := []WildcardListener{
		{Hostname: "*.review.example.com", SecretNamespace: "gateways", SecretName: "review-tls"},
		{Hostname: "*.dev.example.com", SecretNamespace: "cert-manager", SecretName: "dev-tls"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ParseWildcardListeners() = %v, want %v", got, want)
	}

	if got, err := ParseWildcardListeners(""); err != nil || got != nil {
		t.Errorf("ParseWildcardListeners(\"\") = %v, %v, want nil, nil", got, err)
	}
}

func TestParseWildcardListeners_Invalid(t *testing.T) {
	for _, value := range []string{
		"*.review.example.com",
		"review.example.com=gateways/tls",
		"*.Review.example.com=gateways/tls",
		"*.=gateways/tls",
		"*.review.example.com=tls",
		"*.review.example.com=/tls",
		"*.review.example.com=gateways/",
		"*.review.example.com=gateways/tls,*.review.example.com=gateways/other",
	} {
		if _, err := ParseWildcardListeners(value); err == nil {
			t.Errorf("ParseWildcardListeners(%q) succeeded, want error", value)
		}
	}
}

func TestWildcardListenerMatchesOneLabel(t *testing.T) {
	tests := map[string]bool{
		"foo.review.example.com":      true,
		"a-b.review.example.com":      true,
		"a.b.review.example.com":      false,
		"review.example.com":          false,
		"fooreview.example.com":       false,
		"foo.review.example.com.evil": false,
		".review.example.com":         false,
	}
	for host, want := range tests {
		if got := reviewListener.matches(host); got != want {
			t.Errorf("matches(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestListenerNameForWildcardIsOperatorListener(t *testing.T) {
	name := translator.ListenerNameForHostname(reviewWildcard)
	if name != "wildcard-review-example-com" {
		t.Errorf("ListenerNameForHostname() = %q, want wildcard-review-example-com", name)
	}
	listener := (&HTTPRouteReconciler{}).createListenerWithNamespaces(reviewWildcard, []string{"app"}, nil)
	if !translator.IsOperatorListener(listener) {
		t.Error("wildcard listener created by the operator must be recognised as its own")
	}
}

func TestApplyWildcardSectionNamesCollapsesParentRefs(t *testing.T) {
	route := &gatewayv1.HTTPRoute{}
	for _, host := range []string{"a.review.example.com", "b.review.example.com", "web.example.com"} {
		route.Spec.ParentRefs = append(route.Spec.ParentRefs, gatewayv1.ParentReference{
			Name:        "nginx",
			Namespace:   ptr.To(gatewayv1.Namespace(testGatewayNamespace)),
			SectionName: ptr.To(gatewayv1.SectionName(host)),
		})
	}

	applyWildcardSectionNames(route, []WildcardListener{reviewListener})

	sections := make([]string, 0, len(route.Spec.ParentRefs))
	for _, ref := range route.Spec.ParentRefs {
		sections = append(sections, string(*ref.SectionName))
	}
	if len(sections) != 2 || sections[0] != "wildcard-review-example-com" || sections[1] != "web.example.com" {
		t.Errorf("sectionNames = %v, want [wildcard-review-example-com web.example.com]", sections)
	}
}

func newWildcardReconciler(t *testing.T, wildcard WildcardListener, objs ...client.Object) *HTTPRouteReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add networkingv1 to scheme: %v", err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to add gatewayv1 to scheme: %v", err)
	}
	if err := gatewayv1beta1.Install(scheme); err != nil {
		t.Fatalf("failed to add gatewayv1beta1 to scheme: %v", err)
	}
	return &HTTPRouteReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
			WithInterceptorFuncs(interceptor.Funcs{Update: rejectGatewayWithoutListeners}).Build(),
		GatewayNamespace:  testGatewayNamespace,
		WildcardListeners: []WildcardListener{wildcard},
	}
}

func sourcedRoute(name, namespace, host string) gatewayv1.HTTPRoute {
	route := testRoute(name, "nginx", "nginx", host)
	route.Namespace = namespace
	route.Annotations[translator.SourceAnnotation] = namespace + "/web"
	return *route
}

func findListener(gw *gatewayv1.Gateway, name string) *gatewayv1.Listener {
	for i := range gw.Spec.Listeners {
		if string(gw.Spec.Listeners[i].Name) == name {
			return &gw.Spec.Listeners[i]
		}
	}
	return nil
}

func TestReconcileGatewayListeners_GroupsHostnamesIntoWildcardListener(t *testing.T) {
	gw := testGateway("nginx", true, "a.review.example.com", "web.example.com")
	routes := []gatewayv1.HTTPRoute{
		sourcedRoute("a", "app", "a.review.example.com"),
		sourcedRoute("b", "team", "b.review.example.com"),
		sourcedRoute("web", "app", "web.example.com"),
		sourcedRoute("deep", "app", "x.y.review.example.com"),
	}
	r := newWildcardReconciler(t, reviewListener, gw,
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}},
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "web"}})

	if _, err := r.reconcileGatewayListeners(context.Background(), "nginx", routes); err != nil {
		t.Fatalf("reconcileGatewayListeners() error: %v", err)
	}

	got, _ := getGateway(t, r, "nginx")
	names := listenerNames(got)
	want := []string{"wildcard-review-example-com", "web.example.com", "x.y.review.example.com"}
	if len(names) != len(want) {
		t.Errorf("listeners = %v, want %v", names, want)
	}
	for _, name := range want {
		if !names[name] {
			t.Errorf("missing listener %q, got %v", name, names)
		}
	}
	wildcard := findListener(got, "wildcard-review-example-com")
	if wildcard == nil {
		t.FailNow()
	}
	if string(*wildcard.Hostname) != reviewWildcard {
		t.Errorf("wildcard listener hostname = %q, want %q", *wildcard.Hostname, reviewWildcard)
	}
	if ref := wildcard.TLS.CertificateRefs[0]; ref.Name != "review-wildcard-tls" ||
		string(*ref.Namespace) != testGatewayNamespace {
		t.Errorf("wildcard certificateRef = %s/%s, want %s/review-wildcard-tls",
			*ref.Namespace, ref.Name, testGatewayNamespace)
	}
	if values := wildcard.AllowedRoutes.Namespaces.Selector.MatchExpressions[0].Values; len(values) != 2 ||
		values[0] != "app" || values[1] != "team" {
		t.Errorf("wildcard listener namespaces = %v, want [app team]", values)
	}
}

func TestUpdateGatewayListeners_AddsWildcardListenerOnce(t *testing.T) {
	gw := testGateway("nginx", true)
	route := testRoute("web", "nginx", "", "a.review.example.com")
	route.Spec.Hostnames = hostnames("a.review.example.com", "b.review.example.com")
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}}
	r := &HTTPRouteReconciler{WildcardListeners: []WildcardListener{reviewListener}}

	updated, _ := r.updateGatewayListeners(context.Background(), gw, route, ingress)

	if !updated || len(gw.Spec.Listeners) != 1 {
		t.Fatalf("updated = %v, listeners = %v, want one wildcard listener", updated, listenerNames(gw))
	}
	if gw.Spec.Listeners[0].TLS == nil || gw.Spec.Listeners[0].TLS.CertificateRefs[0].Name != "review-wildcard-tls" {
		t.Errorf("wildcard listener TLS = %+v, want review-wildcard-tls", gw.Spec.Listeners[0].TLS)
	}
}

func getReferenceGrant(t *testing.T, r *HTTPRouteReconciler, namespace string) (*gatewayv1beta1.ReferenceGrant, bool) {
	t.Helper()
	grant := &gatewayv1beta1.ReferenceGrant{}
	err := r.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: translator.ReferenceGrantName}, grant)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("failed to get ReferenceGrant: %v", err)
	}
	return grant, true
}

func TestWildcardCertificateInOtherNamespaceGetsReferenceGrant(t *testing.T) {
	foreign := WildcardListener{Hostname: reviewWildcard, SecretNamespace: "cert-manager", SecretName: "review-tls"}
	gw := testGateway("nginx", true, "web.example.com")
	routes := []gatewayv1.HTTPRoute{
		sourcedRoute("a", "app", "a.review.example.com"),
		sourcedRoute("web", "app", "web.example.com"),
	}
	r := newWildcardReconciler(t, foreign, gw,
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}})

	if _, err := r.reconcileGatewayListeners(context.Background(), "nginx", routes); err != nil {
		t.Fatalf("reconcileGatewayListeners() error: %v", err)
	}
	grant, ok := getReferenceGrant(t, r, "cert-manager")
	if !ok {
		t.Fatal("ReferenceGrant in the certificate namespace was not created")
	}
	if from := grant.Spec.From[0]; from.Kind != "Gateway" || string(from.Namespace) != testGatewayNamespace {
		t.Errorf("ReferenceGrant from = %+v, want Gateways in %s", from, testGatewayNamespace)
	}

	if _, err := r.reconcileGatewayListeners(context.Background(), "nginx", routes[1:]); err != nil {
		t.Fatalf("reconcileGatewayListeners() error: %v", err)
	}
	if _, ok := getReferenceGrant(t, r, "cert-manager"); ok {
		t.Error("ReferenceGrant must be deleted once the wildcard listener is gone")
	}
}
