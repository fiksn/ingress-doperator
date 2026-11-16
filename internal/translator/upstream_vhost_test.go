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

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// urlRewriteHostnames returns, per rule of the routes, the hostname of its URLRewrite filter ("" if none).
func urlRewriteHostnames(routes []*gatewayv1.HTTPRoute) []string {
	var hostnames []string
	for _, route := range routes {
		for _, rule := range route.Spec.Rules {
			hostname := ""
			for _, filter := range rule.Filters {
				if filter.Type == gatewayv1.HTTPRouteFilterURLRewrite && filter.URLRewrite != nil &&
					filter.URLRewrite.Hostname != nil {
					hostname = string(*filter.URLRewrite.Hostname)
				}
			}
			hostnames = append(hostnames, hostname)
		}
	}
	return hostnames
}

func TestUpstreamVhost(t *testing.T) {
	tests := []struct {
		name        string
		value       *string
		wantRewrite string
		wantWarning bool
	}{
		{name: "not set"},
		{name: "plain hostname", value: ptrTo("backend.internal.example"), wantRewrite: "backend.internal.example"},
		{name: "uppercase and spaces", value: ptrTo("  Backend.Example.COM "), wantRewrite: "backend.example.com"},
		{name: "single label", value: ptrTo("backend"), wantRewrite: "backend"},
		{name: "empty", value: ptrTo("  ")},
		{name: "nginx variable", value: ptrTo("$host"), wantWarning: true},
		{name: "variable in name", value: ptrTo("$service_name.app.svc"), wantWarning: true},
		{name: "with port", value: ptrTo("backend.example.com:8443"), wantWarning: true},
		{name: "wildcard", value: ptrTo("*.example.com"), wantWarning: true},
		{name: "too long", value: ptrTo(strings.Repeat("a.", 127) + "com"), wantWarning: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ingress := groupedIngress(
				rule("a.example.com", servicePath("/", "a"), servicePath("/api", "api")),
				rule("b.example.com", servicePath("/", "b")),
			)
			if tt.value != nil {
				ingress.Annotations = map[string]string{UpstreamVhostAnnotation: *tt.value}
			}
			trans := New(Config{GatewayName: "shared", GatewayNamespace: "gateways"})

			got := urlRewriteHostnames(trans.TranslateToHTTPRoutesWithCanaries(ingress, nil))

			want := []string{tt.wantRewrite, tt.wantRewrite, tt.wantRewrite}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("URLRewrite hostnames per rule = %q, want %q", got, want)
			}
			warnings := UnsupportedAnnotationWarnings(ingress)
			hasWarning := len(warnings) == 1 && strings.Contains(warnings[0], UpstreamVhostAnnotation)
			if hasWarning != tt.wantWarning || (!tt.wantWarning && len(warnings) > 0) {
				t.Errorf("UnsupportedAnnotationWarnings() = %v, want warning: %v", warnings, tt.wantWarning)
			}
		})
	}
}

func ptrTo(value string) *string { return &value }
