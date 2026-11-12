/*
Copyright 2026.

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

package main

import (
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/controller"
	"github.com/fiksn/ingress-doperator/internal/utils"
)

const testHostname = "api.example.com"

var (
	hostnameKey      = controller.ExternalDNSHostnameAnnotation
	ingressSourceKey = controller.ExternalDNSIngressHostnameSource
	gatewaySourceKey = controller.ExternalDNSGatewayHostnameSource
	annotationOnly   = controller.ExternalDNSHostnameSourceAnnotationOnly
)

func testIngress(annotations map[string]string) *networkingv1.Ingress {
	return &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
		Namespace: "app", Name: "api", Annotations: annotations,
	}}
}

func testRoute(annotations map[string]string) *gatewayv1.HTTPRoute {
	all := map[string]string{
		utils.ManagedByAnnotation: utils.ManagedByValue,
		utils.SourceAnnotation:    "app/api",
	}
	for key, value := range annotations {
		all[key] = value
	}
	return &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace: "app", Name: "api", Annotations: all,
	}}
}

// cutOverIngress is an Ingress after the operator moved its DNS to the HTTPRoute.
func cutOverIngress() *networkingv1.Ingress {
	return testIngress(map[string]string{
		controller.IngressDisabledAnnotation:   controller.IngressDisabledReasonExternalDNS,
		ingressSourceKey:                       annotationOnly,
		hostnameKey:                            testHostname,
		controller.OriginalExternalDNSHostname: testHostname,
	})
}

func runSwitch(t *testing.T, target dnsTarget, objs ...client.Object) (client.Client, error) {
	t.Helper()
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	err := runReenabler(context.Background(), cli, "app", "", reenablerOptions{dnsTarget: target})
	return cli, err
}

func getAnnotations(t *testing.T, cli client.Client, obj client.Object) map[string]string {
	t.Helper()
	if err := cli.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatalf("failed to get %s: %v", obj.GetName(), err)
	}
	return obj.GetAnnotations()
}

func assertAnnotations(t *testing.T, kind string, got map[string]string, want map[string]string) {
	t.Helper()
	for key, value := range want {
		actual, ok := got[key]
		switch {
		case value == "" && ok:
			t.Errorf("%s annotation %q = %q, want it absent", kind, key, actual)
		case value != "" && actual != value:
			t.Errorf("%s annotation %q = %q, want %q", kind, key, actual, value)
		}
	}
}

func TestParseDNSTarget(t *testing.T) {
	for _, value := range []string{"", "ingress", "httproute"} {
		if got, err := parseDNSTarget(value); err != nil || string(got) != value {
			t.Errorf("parseDNSTarget(%q) = %q, %v", value, got, err)
		}
	}
	for _, value := range []string{"Ingress", "gateway", "both"} {
		if _, err := parseDNSTarget(value); err == nil {
			t.Errorf("parseDNSTarget(%q) returned no error", value)
		}
	}
}

func TestDNSTargetIngressRestoresIngressAndShadowsRoute(t *testing.T) {
	ingress := cutOverIngress()
	route := testRoute(map[string]string{hostnameKey: testHostname})

	cli, err := runSwitch(t, dnsTargetIngress, ingress, route)
	if err != nil {
		t.Fatalf("runReenabler() error: %v", err)
	}

	assertAnnotations(t, "Ingress", getAnnotations(t, cli, ingress), map[string]string{
		controller.ShadowExternalDNSAnnotation: "true",
		hostnameKey:                            testHostname,
		ingressSourceKey:                       "",
		controller.IngressDisabledAnnotation:   "",
		controller.OriginalExternalDNSHostname: "",
	})
	assertAnnotations(t, "HTTPRoute", getAnnotations(t, cli, route), map[string]string{
		gatewaySourceKey: annotationOnly,
		hostnameKey:      "",
	})
}

func TestDNSTargetHTTPRouteCutsOverIngressAndUnshadowsRoute(t *testing.T) {
	ingress := testIngress(map[string]string{
		controller.ShadowExternalDNSAnnotation: "true",
		hostnameKey:                            testHostname,
	})
	route := testRoute(map[string]string{gatewaySourceKey: annotationOnly})

	cli, err := runSwitch(t, dnsTargetHTTPRoute, ingress, route)
	if err != nil {
		t.Fatalf("runReenabler() error: %v", err)
	}

	assertAnnotations(t, "Ingress", getAnnotations(t, cli, ingress), map[string]string{
		controller.ShadowExternalDNSAnnotation: "",
		controller.IngressDisabledAnnotation:   controller.IngressDisabledReasonExternalDNS,
		ingressSourceKey:                       annotationOnly,
		controller.OriginalExternalDNSHostname: testHostname,
	})
	assertAnnotations(t, "HTTPRoute", getAnnotations(t, cli, route), map[string]string{gatewaySourceKey: ""})
}

func TestDNSTargetRoundTripRestoresIngress(t *testing.T) {
	ingress := testIngress(map[string]string{hostnameKey: testHostname, ingressSourceKey: "defined-hosts-only"})
	route := testRoute(map[string]string{gatewaySourceKey: annotationOnly})

	cli, err := runSwitch(t, dnsTargetHTTPRoute, ingress, route)
	if err != nil {
		t.Fatalf("runReenabler(httproute) error: %v", err)
	}
	if err := runReenabler(context.Background(), cli, "app", "",
		reenablerOptions{dnsTarget: dnsTargetIngress}); err != nil {
		t.Fatalf("runReenabler(ingress) error: %v", err)
	}

	assertAnnotations(t, "Ingress", getAnnotations(t, cli, ingress), map[string]string{
		hostnameKey:                          testHostname,
		ingressSourceKey:                     "defined-hosts-only",
		controller.IngressDisabledAnnotation: "",
		controller.OriginalExternalDNSIngressHostnameSource: "",
		controller.ShadowExternalDNSAnnotation:              "true",
	})
}

func TestDNSTargetIngressWorksForIgnoredIngress(t *testing.T) {
	annotations := map[string]string{controller.IgnoreIngressAnnotation: "true"}
	for key, value := range cutOverIngress().Annotations {
		annotations[key] = value
	}
	ingress := testIngress(annotations)
	route := testRoute(nil)

	cli, err := runSwitch(t, dnsTargetIngress, ingress, route)
	if err != nil {
		t.Fatalf("runReenabler() error: %v", err)
	}
	assertAnnotations(t, "Ingress", getAnnotations(t, cli, ingress), map[string]string{ingressSourceKey: ""})
	assertAnnotations(t, "HTTPRoute", getAnnotations(t, cli, route), map[string]string{gatewaySourceKey: annotationOnly})
}

func TestDNSTargetRefusals(t *testing.T) {
	classDisabled := map[string]string{controller.IngressDisabledAnnotation: controller.IngressDisabledReasonNormal}
	tests := []struct {
		name        string
		target      dnsTarget
		annotations map[string]string
		withRoute   bool
	}{
		{"httproute without managed route", dnsTargetHTTPRoute, map[string]string{hostnameKey: testHostname}, false},
		{"httproute for ignored ingress", dnsTargetHTTPRoute,
			map[string]string{controller.IgnoreIngressAnnotation: "true"}, true},
		{"httproute for class-disabled ingress", dnsTargetHTTPRoute, classDisabled, true},
		{"ingress for class-disabled ingress", dnsTargetIngress, classDisabled, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ingress := testIngress(tt.annotations)
			objs := []client.Object{ingress}
			if tt.withRoute {
				objs = append(objs, testRoute(map[string]string{gatewaySourceKey: annotationOnly}))
			}

			cli, err := runSwitch(t, tt.target, objs...)
			if err == nil {
				t.Fatal("runReenabler() returned no error")
			}

			got := getAnnotations(t, cli, ingress)
			if len(got) != len(tt.annotations) {
				t.Errorf("Ingress annotations changed: got %v, want %v", got, tt.annotations)
			}
		})
	}
}
