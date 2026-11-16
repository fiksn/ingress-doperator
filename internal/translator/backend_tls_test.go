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
)

func TestBackendTLSFor(t *testing.T) {
	tests := []struct {
		name         string
		annotations  map[string]string
		want         BackendTLS
		wantWarnings []string
	}{
		{"no CA uses system CAs", nil, BackendTLS{}, []string{"system CAs"}},
		{"backend-tls-ca Secret", map[string]string{BackendTLSCAAnnotation: "Secret/web-ca"},
			BackendTLS{CAKind: "Secret", CAName: "web-ca"}, nil},
		{"backend-tls-ca ConfigMap, kind case-insensitive", map[string]string{BackendTLSCAAnnotation: "configmap/web-ca"},
			BackendTLS{CAKind: "ConfigMap", CAName: "web-ca"}, nil},
		{"backend-tls-ca invalid kind", map[string]string{BackendTLSCAAnnotation: "Pod/web-ca"},
			BackendTLS{}, []string{`"Pod/web-ca" is not`, "system CAs"}},
		{"backend-tls-ca invalid name", map[string]string{BackendTLSCAAnnotation: "Secret/Not_Valid"},
			BackendTLS{}, []string{"is not", "system CAs"}},
		{"backend-tls-ca wins over proxy-ssl-secret", map[string]string{
			BackendTLSCAAnnotation: "ConfigMap/web-ca", ProxySSLSecretAnnotation: "app/client"},
			BackendTLS{CAKind: "ConfigMap", CAName: "web-ca"}, nil},
		{"proxy-ssl-secret in namespace", map[string]string{ProxySSLSecretAnnotation: "app/client"},
			BackendTLS{CAKind: "Secret", CAName: "client"}, []string{"client certificate"}},
		{"proxy-ssl-secret without namespace", map[string]string{ProxySSLSecretAnnotation: "client"},
			BackendTLS{CAKind: "Secret", CAName: "client"}, []string{"client certificate"}},
		{"proxy-ssl-secret in other namespace", map[string]string{ProxySSLSecretAnnotation: "other/client"},
			BackendTLS{}, []string{"client certificate", "not in namespace app", "system CAs"}},
		{"proxy-ssl-name", map[string]string{ProxySSLNameAnnotation: "Backend.Example", BackendTLSCAAnnotation: "Secret/ca"},
			BackendTLS{Hostname: "backend.example", CAKind: "Secret", CAName: "ca"}, nil},
		{"upstream-vhost as hostname", map[string]string{UpstreamVhostAnnotation: "vhost.example", BackendTLSCAAnnotation: "Secret/ca"},
			BackendTLS{Hostname: "vhost.example", CAKind: "Secret", CAName: "ca"}, nil},
		{"proxy-ssl-name wins over upstream-vhost", map[string]string{
			ProxySSLNameAnnotation: "sni.example", UpstreamVhostAnnotation: "vhost.example", BackendTLSCAAnnotation: "Secret/ca"},
			BackendTLS{Hostname: "sni.example", CAKind: "Secret", CAName: "ca"}, nil},
		{"invalid proxy-ssl-name", map[string]string{ProxySSLNameAnnotation: "$host", BackendTLSCAAnnotation: "Secret/ca"},
			BackendTLS{CAKind: "Secret", CAName: "ca"}, []string{"not a plain hostname"}},
		{"verify off", map[string]string{ProxySSLVerifyAnnotation: "off", BackendTLSCAAnnotation: "Secret/ca"},
			BackendTLS{CAKind: "Secret", CAName: "ca"}, []string{"always verifies"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ingress := groupedIngress(rule("a.example.com", servicePath("/", "a")))
			ingress.Annotations = map[string]string{BackendProtocolAnnotation: "HTTPS"}
			for key, value := range tt.annotations {
				ingress.Annotations[key] = value
			}

			got, warnings := BackendTLSFor(ingress)

			if got != tt.want {
				t.Errorf("BackendTLSFor() = %+v, want %+v", got, tt.want)
			}
			if len(warnings) != len(tt.wantWarnings) {
				t.Fatalf("warnings = %q, want %d matching %q", warnings, len(tt.wantWarnings), tt.wantWarnings)
			}
			for _, want := range tt.wantWarnings {
				if !strings.Contains(strings.Join(warnings, "\n"), want) {
					t.Errorf("warnings %q do not mention %q", warnings, want)
				}
			}
		})
	}
}
