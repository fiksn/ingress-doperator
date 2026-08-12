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
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ingressWithHostPath(host, path string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "ing", Namespace: "app"},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{Path: path}},
					},
				},
			}},
		},
	}
}

func TestSharesHostPath(t *testing.T) {
	cases := []struct {
		name string
		a, b *networkingv1.Ingress
		want bool
	}{
		{"same host and path", ingressWithHostPath("h", "/"), ingressWithHostPath("h", "/"), true},
		{"same host different path", ingressWithHostPath("h", "/a"), ingressWithHostPath("h", "/b"), false},
		{"different host same path", ingressWithHostPath("h1", "/"), ingressWithHostPath("h2", "/"), false},
		{"empty host both", ingressWithHostPath("", "/"), ingressWithHostPath("", "/"), true},
	}
	for _, tc := range cases {
		if got := sharesHostPath(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: sharesHostPath = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSharesHostPathNilHTTP(t *testing.T) {
	a := &networkingv1.Ingress{
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: "h"}}, // no HTTP block
		},
	}
	if sharesHostPath(a, ingressWithHostPath("h", "/")) {
		t.Error("rule without HTTP block must not match")
	}
}
