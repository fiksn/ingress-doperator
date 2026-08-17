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
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func snippetIngress(name, host string, created time.Time, annotations map[string]string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "app",
			CreationTimestamp: metav1.NewTime(created),
			Annotations:       annotations,
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{Path: "/"}},
					},
				},
			}},
		},
	}
}

func newSnippetConflictReconciler(t *testing.T, ingresses ...*networkingv1.Ingress) *IngressReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add networkingv1 to scheme: %v", err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, ing := range ingresses {
		builder = builder.WithObjects(ing)
	}
	return &IngressReconciler{
		Client:              builder.Build(),
		IngressClassFilters: []string{"*"},
	}
}

func TestDetectSnippetConflicts_YoungerSuppressesServerOnly(t *testing.T) {
	base := time.Unix(1000, 0)
	sslCiphers := map[string]string{"nginx.ingress.kubernetes.io/ssl-ciphers": "HIGH:!aNULL"}
	older := snippetIngress("old", "shared.example.com", base, sslCiphers)
	younger := snippetIngress("new", "shared.example.com", base.Add(time.Hour), sslCiphers)
	r := newSnippetConflictReconciler(t, older, younger)

	if got := r.detectSnippetConflicts(context.Background(), younger); len(got) != 1 {
		t.Fatalf("younger Ingress should suppress ssl-ciphers, got %v", got)
	} else if _, ok := got["ssl-ciphers"]; !ok {
		t.Errorf("expected ssl-ciphers suppressed, got %v", got)
	}

	if got := r.detectSnippetConflicts(context.Background(), older); len(got) != 0 {
		t.Errorf("older Ingress should own the directive (no suppression), got %v", got)
	}
}

func TestDetectSnippetConflicts_DifferentHostNoConflict(t *testing.T) {
	base := time.Unix(1000, 0)
	sslCiphers := map[string]string{"nginx.ingress.kubernetes.io/ssl-ciphers": "HIGH:!aNULL"}
	a := snippetIngress("a", "a.example.com", base, sslCiphers)
	b := snippetIngress("b", "b.example.com", base.Add(time.Hour), sslCiphers)
	r := newSnippetConflictReconciler(t, a, b)

	if got := r.detectSnippetConflicts(context.Background(), b); len(got) != 0 {
		t.Errorf("different hosts must not conflict, got %v", got)
	}
}

func TestDetectSnippetConflicts_LocationDirectiveNotSuppressed(t *testing.T) {
	base := time.Unix(1000, 0)
	timeout := map[string]string{"nginx.ingress.kubernetes.io/proxy-read-timeout": "60s"}
	older := snippetIngress("old", "shared.example.com", base, timeout)
	younger := snippetIngress("new", "shared.example.com", base.Add(time.Hour), timeout)
	r := newSnippetConflictReconciler(t, older, younger)

	if got := r.detectSnippetConflicts(context.Background(), younger); len(got) != 0 {
		t.Errorf("location-scoped proxy-read-timeout must not be suppressed, got %v", got)
	}
}
