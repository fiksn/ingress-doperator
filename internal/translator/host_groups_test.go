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

package translator

import (
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
)

func servicePath(path, service string) networkingv1.HTTPIngressPath {
	return networkingv1.HTTPIngressPath{
		Path:     path,
		PathType: ptr.To(networkingv1.PathTypePrefix),
		Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
			Name: service, Port: networkingv1.ServiceBackendPort{Number: 80},
		}},
	}
}

func rule(host string, paths ...networkingv1.HTTPIngressPath) networkingv1.IngressRule {
	return networkingv1.IngressRule{
		Host:             host,
		IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: paths}},
	}
}

func groupedIngress(rules ...networkingv1.IngressRule) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"},
		Spec:       networkingv1.IngressSpec{Rules: rules},
	}
}

func groupHosts(groups []hostGroup) []string {
	out := make([]string, 0, len(groups))
	for _, group := range groups {
		out = append(out, strings.Join(group.hosts, "+"))
	}
	return out
}

func TestHostGroups(t *testing.T) {
	tests := []struct {
		name    string
		ingress *networkingv1.Ingress
		want    []string
	}{
		{"one host", groupedIngress(rule("a.example.com", servicePath("/", "a"))), []string{"a.example.com"}},
		{"same paths", groupedIngress(
			rule("a.example.com", servicePath("/", "web")),
			rule("b.example.com", servicePath("/", "web")),
		), []string{"a.example.com+b.example.com"}},
		{"different backends", groupedIngress(
			rule("a.example.com", servicePath("/", "a")),
			rule("b.example.com", servicePath("/", "b")),
		), []string{"a.example.com", "b.example.com"}},
		{"different paths", groupedIngress(
			rule("a.example.com", servicePath("/", "web")),
			rule("b.example.com", servicePath("/", "web"), servicePath("/admin", "admin")),
		), []string{"a.example.com", "b.example.com"}},
		{"groups in order of first appearance", groupedIngress(
			rule("a.example.com", servicePath("/", "a")),
			rule("b.example.com", servicePath("/", "b")),
			rule("c.example.com", servicePath("/", "a")),
		), []string{"a.example.com+c.example.com", "b.example.com"}},
		{"host in several rules gets all its paths", groupedIngress(
			rule("a.example.com", servicePath("/", "web")),
			rule("b.example.com", servicePath("/", "web")),
			rule("a.example.com", servicePath("/admin", "admin")),
		), []string{"a.example.com", "b.example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := groupHosts(hostGroups(tt.ingress))
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("hostGroups() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHostGroups_PathsWithoutHostApplyToEveryHost(t *testing.T) {
	groups := hostGroups(groupedIngress(
		rule("a.example.com", servicePath("/", "a")),
		rule("", servicePath("/health", "health")),
		rule("b.example.com", servicePath("/", "b")),
	))

	if len(groups) != 2 {
		t.Fatalf("hostGroups() = %v, want two groups", groupHosts(groups))
	}
	for _, group := range groups {
		last := group.paths[len(group.paths)-1]
		if last.Path != "/health" || last.Backend.Service.Name != "health" {
			t.Errorf("group %v does not end with the host-less /health path: %+v", group.hosts, group.paths)
		}
	}
}

func TestTranslateToHTTPRoutesWithCanaries_SingleGroupIsUnchanged(t *testing.T) {
	trans := New(Config{GatewayName: "shared", GatewayNamespace: "gateways"})
	ingress := groupedIngress(
		rule("a.example.com", servicePath("/", "web")),
		rule("b.example.com", servicePath("/", "web")),
	)

	routes := trans.TranslateToHTTPRoutesWithCanaries(ingress, nil)

	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}
	if want := trans.TranslateToHTTPRouteWithCanaries(ingress, nil); !equality.Semantic.DeepEqual(routes[0], want) {
		t.Errorf("single-group route differs from TranslateToHTTPRouteWithCanaries:\n got %+v\nwant %+v",
			routes[0].Spec, want.Spec)
	}
}

func TestTranslateToHTTPRoutesWithCanaries_SplitsByHost(t *testing.T) {
	trans := New(Config{GatewayName: "shared", GatewayNamespace: "gateways"})
	ingress := groupedIngress(
		rule("a.example.com", servicePath("/", "a")),
		rule("b.example.com", servicePath("/", "b")),
	)

	routes := trans.TranslateToHTTPRoutesWithCanaries(ingress, nil)

	want := []struct {
		name, host, service string
	}{
		{"web", "a.example.com", "a"},
		{"web-b.example.com", "b.example.com", "b"},
	}
	if len(routes) != len(want) {
		t.Fatalf("got %d routes, want %d", len(routes), len(want))
	}
	for i, w := range want {
		route := routes[i]
		if route.Name != w.name {
			t.Errorf("route %d name = %q, want %q", i, route.Name, w.name)
		}
		if len(route.Spec.Hostnames) != 1 || string(route.Spec.Hostnames[0]) != w.host {
			t.Errorf("route %s hostnames = %v, want [%s]", route.Name, route.Spec.Hostnames, w.host)
		}
		if len(route.Spec.ParentRefs) != 1 || string(ptr.Deref(route.Spec.ParentRefs[0].SectionName, "")) != w.host {
			t.Errorf("route %s parentRefs = %+v, want one for section %s", route.Name, route.Spec.ParentRefs, w.host)
		}
		if len(route.Spec.Rules) != 1 || string(route.Spec.Rules[0].BackendRefs[0].Name) != w.service {
			t.Errorf("route %s rules = %+v, want one rule to %s", route.Name, route.Spec.Rules, w.service)
		}
		if route.Annotations[SourceAnnotation] != "app/web" {
			t.Errorf("route %s source = %q, want app/web", route.Name, route.Annotations[SourceAnnotation])
		}
	}
}

func TestHostGroupRouteName(t *testing.T) {
	long := strings.Repeat("a", 240)
	tests := []struct {
		ingress, host string
		want          string
	}{
		{"web", "api.example.com", "web-api.example.com"},
		{"web", "API.Example.com", "web-api.example.com"},
		{"web", "*.example.com", "web-wildcard.example.com"},
	}
	for _, tt := range tests {
		if got := hostGroupRouteName(tt.ingress, tt.host); got != tt.want {
			t.Errorf("hostGroupRouteName(%q, %q) = %q, want %q", tt.ingress, tt.host, got, tt.want)
		}
	}

	name := hostGroupRouteName(long, "api.example.com")
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		t.Errorf("hostGroupRouteName() = %q is not a valid name: %v", name, errs)
	}
	if !strings.HasPrefix(name, long) {
		t.Errorf("hostGroupRouteName() = %q does not start with the Ingress name", name)
	}
	if name == hostGroupRouteName(long, "api.example.org") {
		t.Error("truncated names of different hosts must differ")
	}
}
