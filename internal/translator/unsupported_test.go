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
)

func TestUnsupportedAnnotationWarnings_BackendProtocol(t *testing.T) {
	tests := []struct {
		protocol     string
		wantWarning  bool
		wantContains []string
	}{
		{protocol: "", wantWarning: false},
		{protocol: "HTTP", wantWarning: false},
		{protocol: "http", wantWarning: false},
		{protocol: "HTTPS", wantWarning: false},
		{protocol: "https", wantWarning: false},
		{protocol: "GRPCS", wantWarning: true, wantContains: []string{"GRPCS is not translated", "Service(s) a, b"}},
		{protocol: "FCGI", wantWarning: true, wantContains: []string{"FCGI is not translated"}},
	}
	for _, tt := range tests {
		t.Run(tt.protocol, func(t *testing.T) {
			ingress := groupedIngress(
				rule("a.example.com", servicePath("/", "a")),
				rule("b.example.com", servicePath("/", "b"), servicePath("/x", "a")),
			)
			if tt.protocol != "" {
				ingress.Annotations = map[string]string{BackendProtocolAnnotation: tt.protocol}
			}

			warnings := UnsupportedAnnotationWarnings(ingress)

			if got := len(warnings) > 0; got != tt.wantWarning {
				t.Fatalf("UnsupportedAnnotationWarnings() = %v, want warning: %v", warnings, tt.wantWarning)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(warnings[0], want) {
					t.Errorf("warning %q does not contain %q", warnings[0], want)
				}
			}
		})
	}
}

func TestUnsupportedAnnotationWarnings_IncludesDefaultBackend(t *testing.T) {
	ingress := groupedIngress(rule("a.example.com", servicePath("/", "a")))
	ingress.Annotations = map[string]string{BackendProtocolAnnotation: "GRPC"}
	ingress.Spec.DefaultBackend = &networkingv1.IngressBackend{
		Service: &networkingv1.IngressServiceBackend{Name: "fallback"},
	}

	warnings := UnsupportedAnnotationWarnings(ingress)

	if len(warnings) != 1 || !strings.Contains(warnings[0], "Service(s) a, fallback") {
		t.Errorf("UnsupportedAnnotationWarnings() = %v, want the default backend listed", warnings)
	}
}
