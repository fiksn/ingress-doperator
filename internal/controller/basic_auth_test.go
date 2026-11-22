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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/translator"
	"github.com/fiksn/ingress-doperator/internal/utils"
)

var authenticationFilterGVK = schema.GroupVersionKind{
	Group: utils.NginxGatewayGroup, Version: "v1alpha1", Kind: utils.AuthenticationFilterKind,
}

func authenticationFilterCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: utils.AuthenticationFilterCRDName},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: true, Storage: true}},
		},
	}
}

func basicAuthIngress(annotations map[string]string) *networkingv1.Ingress {
	ingress := multiHostIngress(hostRule(consoleHost,
		ingressPath("/", networkingv1.PathTypePrefix, "site", numberedPort(80)),
		ingressPath("/admin", networkingv1.PathTypePrefix, "site", numberedPort(80))))
	ingress.TypeMeta = metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"}
	ingress.Annotations = annotations
	return ingress
}

func staleRoute(ingress *networkingv1.Ingress) *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace: ingress.Namespace, Name: ingress.Name,
		Annotations: map[string]string{
			translator.ManagedByAnnotation: translator.ManagedByValue,
			translator.SourceAnnotation:    ingress.Namespace + "/" + ingress.Name,
		},
	}}
}

func getAuthenticationFilter(t *testing.T, r *IngressReconciler, namespace, name string) (*unstructured.Unstructured, bool) {
	t.Helper()
	filter := &unstructured.Unstructured{}
	filter.SetGroupVersionKind(authenticationFilterGVK)
	err := r.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, filter)
	if err != nil {
		return nil, false
	}
	return filter, true
}

func authFiltersOf(rule gatewayv1.HTTPRouteRule) []string {
	var names []string
	for _, filter := range rule.Filters {
		if filter.Type == gatewayv1.HTTPRouteFilterExtensionRef &&
			string(filter.ExtensionRef.Kind) == utils.AuthenticationFilterKind {
			names = append(names, string(filter.ExtensionRef.Name))
		}
	}
	return names
}

func TestBasicAuth_GeneratesAuthenticationFilterForEveryRule(t *testing.T) {
	ingress := basicAuthIngress(map[string]string{
		utils.AuthTypeAnnotation:   "basic",
		utils.AuthSecretAnnotation: multiHostNamespace + "/basic-auth",
		utils.AuthRealmAnnotation:  "Authentication Required",
	})
	r := newTranslationReconciler(t, ingress, clusterIPService("site"), authenticationFilterCRD())

	reconcileIngress(t, r, ingress)

	filterName := utils.BasicAuthFilterName(ingress.Name)
	filter, ok := getAuthenticationFilter(t, r, ingress.Namespace, filterName)
	if !ok {
		t.Fatalf("AuthenticationFilter %s was not created", filterName)
	}
	spec, _, _ := unstructured.NestedMap(filter.Object, "spec")
	secret, _, _ := unstructured.NestedString(spec, "basic", "secretRef", "name")
	realm, _, _ := unstructured.NestedString(spec, "basic", "realm")
	if spec["type"] != "Basic" || secret != "basic-auth" || realm != "Authentication Required" {
		t.Errorf("unexpected AuthenticationFilter spec: %v", spec)
	}
	if !utils.IsManagedByUsForIngress(filter, ingress.Namespace, ingress.Name) ||
		len(filter.GetOwnerReferences()) != 1 || filter.GetOwnerReferences()[0].Name != ingress.Name {
		t.Errorf("AuthenticationFilter is not managed by and owned by the Ingress: %v %v",
			filter.GetAnnotations(), filter.GetOwnerReferences())
	}
	rules := 0
	for _, route := range ingressRoutes(t, r, ingress) {
		for _, rule := range route.Spec.Rules {
			rules++
			if got := authFiltersOf(rule); len(got) != 1 || got[0] != filterName {
				t.Errorf("route %s rule %v has AuthenticationFilters %v, want [%s]", route.Name, rule.Matches, got, filterName)
			}
		}
	}
	if rules == 0 {
		t.Fatal("no HTTPRoute rules generated")
	}
}

func TestBasicAuth_UntranslatableSkipsIngress(t *testing.T) {
	unmanaged := &unstructured.Unstructured{}
	unmanaged.SetGroupVersionKind(authenticationFilterGVK)
	unmanaged.SetNamespace(multiHostNamespace)
	unmanaged.SetName(utils.BasicAuthFilterName("objects"))

	tests := []struct {
		name        string
		annotations map[string]string
		objs        []client.Object
		reason      string
	}{
		{name: "digest",
			annotations: map[string]string{utils.AuthTypeAnnotation: "digest", utils.AuthSecretAnnotation: "creds"},
			reason:      "only basic authentication"},
		{name: "secret in another namespace",
			annotations: map[string]string{utils.AuthTypeAnnotation: "basic", utils.AuthSecretAnnotation: "other/creds"},
			reason:      "another namespace"},
		{name: "auth-map",
			annotations: map[string]string{utils.AuthTypeAnnotation: "basic", utils.AuthSecretAnnotation: "creds",
				utils.AuthSecretTypeAnnotation: "auth-map"},
			reason: "auth-file"},
		{name: "filter owned by someone else",
			annotations: map[string]string{utils.AuthTypeAnnotation: "basic", utils.AuthSecretAnnotation: "creds"},
			objs:        []client.Object{unmanaged},
			reason:      "not managed by ingress-doperator"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ingress := basicAuthIngress(tt.annotations)
			objs := append([]client.Object{ingress, clusterIPService("site"), authenticationFilterCRD(), staleRoute(ingress)},
				tt.objs...)
			recorder := events.NewFakeRecorder(10)
			r := newTranslationReconciler(t, objs...)
			r.Recorder = recorder

			result := reconcileIngress(t, r, ingress)

			if result.RequeueAfter != basicAuthRetryInterval {
				t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, basicAuthRetryInterval)
			}
			if routes := routesOf(t, r, ingress); len(routes) != 0 {
				t.Errorf("got %d HTTPRoutes serving the hosts without authentication, want none", len(routes))
			}
			got := drainEvents(recorder)
			if !strings.Contains(got, "Warning BasicAuthUnsupported") || !strings.Contains(got, tt.reason) {
				t.Errorf("no BasicAuthUnsupported warning mentioning %q:\n%s", tt.reason, got)
			}
		})
	}
}

func TestBasicAuth_ExplicitAuthenticationFilterWins(t *testing.T) {
	ingress := basicAuthIngress(map[string]string{
		utils.AuthTypeAnnotation:          "basic",
		utils.AuthSecretAnnotation:        "creds",
		HTTPRouteAuthenticationAnnotation: "sso",
	})
	recorder := events.NewFakeRecorder(10)
	r := newTranslationReconciler(t, ingress, clusterIPService("site"), authenticationFilterCRD())
	r.Recorder = recorder

	reconcileIngress(t, r, ingress)

	if _, ok := getAuthenticationFilter(t, r, ingress.Namespace, utils.BasicAuthFilterName(ingress.Name)); ok {
		t.Error("basic auth AuthenticationFilter generated although httproute-authentication-filter is set")
	}
	if len(ingressRoutes(t, r, ingress)) == 0 {
		t.Error("Ingress not translated")
	}
	if got := drainEvents(recorder); !strings.Contains(got, "Warning BasicAuthIgnored") {
		t.Errorf("no BasicAuthIgnored warning:\n%s", got)
	}
}

func TestBasicAuth_NotRequestedGeneratesNothing(t *testing.T) {
	ingress := basicAuthIngress(map[string]string{utils.AuthSecretAnnotation: "creds"})
	r := newTranslationReconciler(t, ingress, clusterIPService("site"), authenticationFilterCRD())

	reconcileIngress(t, r, ingress)

	if _, ok := getAuthenticationFilter(t, r, ingress.Namespace, utils.BasicAuthFilterName(ingress.Name)); ok {
		t.Error("AuthenticationFilter generated without auth-type")
	}
	for _, route := range ingressRoutes(t, r, ingress) {
		for _, rule := range route.Spec.Rules {
			if got := authFiltersOf(rule); len(got) != 0 {
				t.Errorf("rule references AuthenticationFilters %v without auth-type", got)
			}
		}
	}
}
