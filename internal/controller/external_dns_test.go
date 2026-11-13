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

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/translator"
)

func TestDisableExternalDNSUsesConfiguredPrefix(t *testing.T) {
	tests := []struct {
		prefix string
		other  string
	}{
		{prefix: DefaultExternalDNSAnnotationPrefix, other: LegacyExternalDNSAnnotationPrefix},
		{prefix: LegacyExternalDNSAnnotationPrefix, other: DefaultExternalDNSAnnotationPrefix},
	}
	t.Cleanup(func() { SetExternalDNSAnnotationPrefix(DefaultExternalDNSAnnotationPrefix) })

	for _, tt := range tests {
		t.Run(tt.prefix, func(t *testing.T) {
			SetExternalDNSAnnotationPrefix(tt.prefix)

			scheme := runtime.NewScheme()
			if err := networkingv1.AddToScheme(scheme); err != nil {
				t.Fatalf("failed to add networkingv1 to scheme: %v", err)
			}
			ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
				Namespace: "ns",
				Name:      "web",
				Annotations: map[string]string{
					tt.prefix + "/hostname": "web.example.com",
					tt.other + "/hostname":  "ignored.example.com",
				},
			}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ingress).Build()

			if err := DisableExternalDNS(context.Background(), c, ingress); err != nil {
				t.Fatalf("DisableExternalDNS returned error: %v", err)
			}

			got := &networkingv1.Ingress{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(ingress), got); err != nil {
				t.Fatalf("failed to get Ingress: %v", err)
			}
			if v := got.Annotations[tt.prefix+"/ingress-hostname-source"]; v != ExternalDNSHostnameSourceAnnotationOnly {
				t.Errorf("ingress-hostname-source under %q = %q, want %q",
					tt.prefix, v, ExternalDNSHostnameSourceAnnotationOnly)
			}
			if _, ok := got.Annotations[tt.other+"/ingress-hostname-source"]; ok {
				t.Errorf("unexpected ingress-hostname-source under %q", tt.other)
			}
			if v := got.Annotations[OriginalExternalDNSHostname]; v != "web.example.com" {
				t.Errorf("%s = %q, want %q", OriginalExternalDNSHostname, v, "web.example.com")
			}
		})
	}
}

func TestShadowExternalDNSHidesRouteFromExternalDNS(t *testing.T) {
	for _, prefix := range []string{DefaultExternalDNSAnnotationPrefix, LegacyExternalDNSAnnotationPrefix} {
		t.Run(prefix, func(t *testing.T) {
			SetExternalDNSAnnotationPrefix(prefix)
			t.Cleanup(func() { SetExternalDNSAnnotationPrefix(DefaultExternalDNSAnnotationPrefix) })

			route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				DefaultExternalDNSAnnotationPrefix + "/hostname":                "web.example.com",
				LegacyExternalDNSAnnotationPrefix + "/hostname":                 "web.example.com",
				LegacyExternalDNSAnnotationPrefix + "/cloudflare-proxied":       "true",
				DefaultExternalDNSAnnotationPrefix + "/ingress-hostname-source": "defined-hosts-only",
				"cert-manager.io/cluster-issuer":                                "letsencrypt",
			}}}

			ShadowExternalDNS(route)

			want := map[string]string{
				prefix + "/gateway-hostname-source": ExternalDNSHostnameSourceAnnotationOnly,
				"cert-manager.io/cluster-issuer":    "letsencrypt",
			}
			if len(route.Annotations) != len(want) {
				t.Errorf("annotations = %v, want %v", route.Annotations, want)
			}
			for key, value := range want {
				if route.Annotations[key] != value {
					t.Errorf("annotation %q = %q, want %q", key, route.Annotations[key], value)
				}
			}
		})
	}
}

func TestShadowExternalDNSWithoutAnnotations(t *testing.T) {
	route := &gatewayv1.HTTPRoute{}

	ShadowExternalDNS(route)

	if v := route.Annotations[ExternalDNSGatewayHostnameSource]; v != ExternalDNSHostnameSourceAnnotationOnly {
		t.Errorf("%s = %q, want %q", ExternalDNSGatewayHostnameSource, v, ExternalDNSHostnameSourceAnnotationOnly)
	}
}

func TestResolveIngressPostProcessingMode(t *testing.T) {
	const (
		shadow     = IngressPostProcessingModeShadowExternalDNS
		disableDNS = IngressPostProcessingModeDisableExternalDNS
	)
	tests := []struct {
		name     string
		global   IngressPostProcessingMode
		disabled string
		shadow   string
		want     IngressPostProcessingMode
	}{
		{"no annotations use global", shadow, "", "", shadow},
		{"disabled=external-dns overrides shadow", shadow, IngressDisabledReasonExternalDNS, "", disableDNS},
		{"disabled=normal overrides shadow", shadow, IngressDisabledReasonNormal, "",
			IngressPostProcessingModeDisable},
		{"shadow overrides disable-external-dns", disableDNS, "", "true", shadow},
		{"shadow overrides none", IngressPostProcessingModeNone, "", "true", shadow},
		{"shadow=false uses global", disableDNS, "", "false", disableDNS},
		{"disabled marker wins over shadow", disableDNS, IngressDisabledReasonExternalDNS, "true", disableDNS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}}
			if tt.disabled != "" {
				ingress.Annotations[IngressDisabledAnnotation] = tt.disabled
			}
			if tt.shadow != "" {
				ingress.Annotations[ShadowExternalDNSAnnotation] = tt.shadow
			}
			if got := resolveIngressPostProcessingMode(tt.global, ingress); got != tt.want {
				t.Errorf("resolveIngressPostProcessingMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestHTTPRouteReconcilerDisablesExternalDNSPerIngress covers cutting over Ingresses whose route
// the Gateway already serves (e.g. after a shadow-external-dns phase): only Ingresses whose
// effective mode is disable-external-dns, and that are not ignored, are hidden from external-dns.
func TestHTTPRouteReconcilerDisablesExternalDNSPerIngress(t *testing.T) {
	marked := map[string]string{IngressDisabledAnnotation: IngressDisabledReasonExternalDNS}
	tests := []struct {
		name        string
		globalMode  IngressPostProcessingMode
		annotations map[string]string
		wantSource  string
	}{
		{"shadow, marked", IngressPostProcessingModeShadowExternalDNS, marked,
			ExternalDNSHostnameSourceAnnotationOnly},
		{"shadow, unmarked", IngressPostProcessingModeShadowExternalDNS, nil, ""},
		{"none, marked", IngressPostProcessingModeNone, marked, ExternalDNSHostnameSourceAnnotationOnly},
		{"disable-external-dns, unmarked", IngressPostProcessingModeDisableExternalDNS, nil,
			ExternalDNSHostnameSourceAnnotationOnly},
		{"disable-external-dns, shadow annotation", IngressPostProcessingModeDisableExternalDNS,
			map[string]string{ShadowExternalDNSAnnotation: "true"}, ""},
		{"disable-external-dns, ignored", IngressPostProcessingModeDisableExternalDNS,
			map[string]string{IgnoreIngressAnnotation: "true"}, ""},
		{"marked, ignored", IngressPostProcessingModeShadowExternalDNS,
			map[string]string{IngressDisabledAnnotation: IngressDisabledReasonExternalDNS,
				IgnoreIngressAnnotation: "true"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ingress := &networkingv1.Ingress{
				TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "app", Name: "api", UID: "api-uid", Annotations: tt.annotations,
				},
				Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
					Host: "api.example.com",
				}}},
			}
			route := ingressRoute(ingress, acceptedStatus)
			gateway := testGateway("nginx", true, "api.example.com")
			r := newReleaseReconciler(t, ingress, route, gateway)
			r.IngressPostProcessingMode = tt.globalMode
			r.gatewayUpdateDebouncer = newGatewayUpdateDebouncer(r)

			ctx := context.Background()
			if _, err := r.handleHTTPRouteCreateOrUpdate(ctx, route); err != nil {
				t.Fatalf("handleHTTPRouteCreateOrUpdate() error: %v", err)
			}

			got := &networkingv1.Ingress{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(ingress), got); err != nil {
				t.Fatalf("failed to get Ingress: %v", err)
			}
			if v := got.Annotations[ExternalDNSIngressHostnameSource]; v != tt.wantSource {
				t.Errorf("%s = %q, want %q", ExternalDNSIngressHostnameSource, v, tt.wantSource)
			}
		})
	}
}

func cutOverTestObjects() (*networkingv1.Ingress, *gatewayv1.HTTPRoute, *gatewayv1.Gateway) {
	ingress := &networkingv1.Ingress{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "app", Name: "api", UID: "api-uid",
			Annotations: map[string]string{IngressDisabledAnnotation: IngressDisabledReasonExternalDNS},
		},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "api.example.com"}}},
	}
	return ingress, ingressRoute(ingress, acceptedStatus), testGateway("nginx", true, "api.example.com")
}

var (
	acceptedStatus = metav1.Condition{
		Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue, Reason: "Accepted",
	}
	noMatchingParentStatus = metav1.Condition{
		Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionFalse,
		Reason: "NoMatchingParent", Message: "The Listener is not found for this parent ref",
	}
)

// ingressRoute returns the managed HTTPRoute of the Ingress on Gateway "nginx", with the given
// Accepted condition reported for that parent (none when accepted.Type is empty).
func ingressRoute(ingress *networkingv1.Ingress, accepted metav1.Condition) *gatewayv1.HTTPRoute {
	route := testRoute("api", "nginx", "nginx", "api.example.com")
	route.Annotations[translator.SourceAnnotation] = ingress.Namespace + "/" + ingress.Name
	route.Generation = 2
	setHTTPRouteOwnerReference(route, ingress)
	if accepted.Type != "" {
		if accepted.ObservedGeneration == 0 {
			accepted.ObservedGeneration = route.Generation
		}
		route.Status.Parents = []gatewayv1.RouteParentStatus{{
			ParentRef:      route.Spec.ParentRefs[0],
			ControllerName: "gateway.nginx.org/nginx-gateway-controller",
			Conditions:     []metav1.Condition{accepted},
		}}
	}
	return route
}

// TestHTTPRouteReconcilerWaitsForAcceptedRoute covers a cut-over whose route the Gateway has not
// accepted (e.g. its listener was never added because the Gateway is full): external-dns would not
// publish the route, so the Ingress must keep its DNS records and the route is requeued.
func TestHTTPRouteReconcilerWaitsForAcceptedRoute(t *testing.T) {
	stale := acceptedStatus
	stale.ObservedGeneration = 1
	tests := []struct {
		name     string
		accepted metav1.Condition
	}{
		{"not accepted", noMatchingParentStatus},
		{"no status yet", metav1.Condition{}},
		{"status of an older generation", stale},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ingress, _, gateway := cutOverTestObjects()
			route := ingressRoute(ingress, tt.accepted)
			r := newReleaseReconciler(t, ingress, route, gateway)
			r.IngressPostProcessingMode = IngressPostProcessingModeShadowExternalDNS
			r.gatewayUpdateDebouncer = newGatewayUpdateDebouncer(r)

			ctx := context.Background()
			result, err := r.handleHTTPRouteCreateOrUpdate(ctx, route)
			if err != nil {
				t.Fatalf("handleHTTPRouteCreateOrUpdate() error: %v", err)
			}
			if result.RequeueAfter == 0 {
				t.Error("handleHTTPRouteCreateOrUpdate() did not requeue the route")
			}

			got := &networkingv1.Ingress{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(ingress), got); err != nil {
				t.Fatalf("failed to get Ingress: %v", err)
			}
			if v, ok := got.Annotations[ExternalDNSIngressHostnameSource]; ok {
				t.Errorf("Ingress was hidden from external-dns (%s=%q) although its route is not accepted",
					ExternalDNSIngressHostnameSource, v)
			}
		})
	}
}

func TestRouteAccepted(t *testing.T) {
	ingress, _, _ := cutOverTestObjects()
	other := gatewayv1.ParentReference{Name: "other", Namespace: ptr.To(gatewayv1.Namespace(testGatewayNamespace))}
	tests := []struct {
		name       string
		route      func() *gatewayv1.HTTPRoute
		want       bool
		wantReason string
	}{
		{"accepted", func() *gatewayv1.HTTPRoute { return ingressRoute(ingress, acceptedStatus) }, true, ""},
		{"rejected", func() *gatewayv1.HTTPRoute { return ingressRoute(ingress, noMatchingParentStatus) },
			false, "NoMatchingParent: The Listener is not found for this parent ref"},
		{"no status", func() *gatewayv1.HTTPRoute { return ingressRoute(ingress, metav1.Condition{}) },
			false, "has not reported a status"},
		{"second parent without status", func() *gatewayv1.HTTPRoute {
			route := ingressRoute(ingress, acceptedStatus)
			route.Spec.ParentRefs = append(route.Spec.ParentRefs, other)
			return route
		}, false, "gateways/other has not reported a status"},
		{"status for a different section", func() *gatewayv1.HTTPRoute {
			route := ingressRoute(ingress, acceptedStatus)
			route.Spec.ParentRefs[0].SectionName = ptr.To(gatewayv1.SectionName("api-example-com"))
			return route
		}, false, "has not reported a status"},
		{"non-Gateway parent is ignored", func() *gatewayv1.HTTPRoute {
			route := ingressRoute(ingress, acceptedStatus)
			route.Spec.ParentRefs = append(route.Spec.ParentRefs, gatewayv1.ParentReference{
				Kind: ptr.To(gatewayv1.Kind("Service")), Group: ptr.To(gatewayv1.Group("")), Name: "mesh",
			})
			return route
		}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := RouteAccepted(tt.route())
			if got != tt.want {
				t.Errorf("RouteAccepted() = %v (%s), want %v", got, reason, tt.want)
			}
			if !strings.Contains(reason, tt.wantReason) {
				t.Errorf("RouteAccepted() reason = %q, want it to contain %q", reason, tt.wantReason)
			}
		})
	}
}

func TestIngressRoutesAcceptedWithoutRoute(t *testing.T) {
	ingress, _, _ := cutOverTestObjects()
	c := fake.NewClientBuilder().WithScheme(newExternalDNSTestScheme(t)).WithObjects(ingress).Build()

	accepted, reason, err := IngressRoutesAccepted(context.Background(), c, ingress)
	if err != nil || accepted || !strings.Contains(reason, "no HTTPRoute") {
		t.Errorf("IngressRoutesAccepted() = %v, %q, %v; want false with a no-HTTPRoute reason", accepted, reason, err)
	}
}

// TestDisableExternalDNSRetriesConflicts simulates another writer changing the Ingress right
// before DisableExternalDNS writes it.
func TestDisableExternalDNSRetriesConflicts(t *testing.T) {
	const otherKey = "other-writer"
	ingress, _, _ := cutOverTestObjects()
	concurrentWrites := 0
	c := fake.NewClientBuilder().WithScheme(newExternalDNSTestScheme(t)).WithObjects(ingress).
		WithInterceptorFuncs(interceptor.Funcs{Update: func(
			ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption,
		) error {
			if concurrentWrites == 0 {
				concurrentWrites++
				latest := &networkingv1.Ingress{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), latest); err != nil {
					return err
				}
				latest.Annotations[otherKey] = "true"
				if err := c.Update(ctx, latest); err != nil {
					return err
				}
			}
			return c.Update(ctx, obj, opts...)
		}}).Build()

	if err := DisableExternalDNS(context.Background(), c, ingress); err != nil {
		t.Fatalf("DisableExternalDNS() error: %v", err)
	}

	got := &networkingv1.Ingress{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ingress), got); err != nil {
		t.Fatalf("failed to get Ingress: %v", err)
	}
	if v := got.Annotations[ExternalDNSIngressHostnameSource]; v != ExternalDNSHostnameSourceAnnotationOnly {
		t.Errorf("%s = %q, want %q", ExternalDNSIngressHostnameSource, v, ExternalDNSHostnameSourceAnnotationOnly)
	}
	if got.Annotations[otherKey] != "true" {
		t.Errorf("concurrent write to %q was lost", otherKey)
	}
}

func TestDisableExternalDNSIgnoresMissingIngress(t *testing.T) {
	ingress, _, _ := cutOverTestObjects()
	c := fake.NewClientBuilder().WithScheme(newExternalDNSTestScheme(t)).Build()

	if err := DisableExternalDNS(context.Background(), c, ingress); err != nil {
		t.Fatalf("DisableExternalDNS() error: %v", err)
	}
}

// TestHTTPRouteReconcilerRequeuesWhenDisableExternalDNSFails covers the cut-over path where the
// Gateway already serves the route: a failure must be returned so the HTTPRoute is requeued.
func TestHTTPRouteReconcilerRequeuesWhenDisableExternalDNSFails(t *testing.T) {
	ingress, route, gateway := cutOverTestObjects()
	r := &HTTPRouteReconciler{
		Client: fake.NewClientBuilder().WithScheme(newExternalDNSTestScheme(t)).
			WithObjects(ingress, route, gateway).
			WithInterceptorFuncs(interceptor.Funcs{Update: func(
				ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption,
			) error {
				if _, ok := obj.(*networkingv1.Ingress); ok {
					return apierrors.NewForbidden(networkingv1.Resource("ingresses"), obj.GetName(), nil)
				}
				return c.Update(ctx, obj, opts...)
			}}).Build(),
		GatewayNamespace:          testGatewayNamespace,
		IngressPostProcessingMode: IngressPostProcessingModeShadowExternalDNS,
	}
	r.gatewayUpdateDebouncer = newGatewayUpdateDebouncer(r)

	if _, err := r.handleHTTPRouteCreateOrUpdate(context.Background(), route); err == nil {
		t.Fatal("handleHTTPRouteCreateOrUpdate() returned no error, want one so the route is requeued")
	}
}

func newExternalDNSTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add networkingv1 to scheme: %v", err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to add gatewayv1 to scheme: %v", err)
	}
	return scheme
}
