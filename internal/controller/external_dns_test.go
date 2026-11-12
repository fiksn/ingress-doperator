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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
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
			route := testRoute("api", "nginx", "nginx", "api.example.com")
			setHTTPRouteOwnerReference(route, ingress)
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
