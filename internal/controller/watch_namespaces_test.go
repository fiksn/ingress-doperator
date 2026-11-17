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
	"slices"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func namespacedIngress(namespace, name string) *networkingv1.Ingress {
	return &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
}

func TestParseNamespacePatterns(t *testing.T) {
	tests := []struct {
		value   string
		want    []string
		wantErr bool
	}{
		{value: "", want: nil},
		{value: "  ", want: nil},
		{value: "a", want: []string{"a"}},
		{value: " a , team-* ,", want: []string{"a", "team-*"}},
		{value: "a,a", want: []string{"a"}},
		{value: "team-[", wantErr: true},
		{value: "ok,bad[", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseNamespacePatterns(tt.value)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseNamespacePatterns(%q) = %v, want error", tt.value, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseNamespacePatterns(%q) unexpected error: %v", tt.value, err)
			continue
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("ParseNamespacePatterns(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}
}

func TestNamespaceFilter(t *testing.T) {
	tests := []struct {
		name      string
		watched   []string
		namespace string
		want      bool
	}{
		{name: "empty list watches all", watched: nil, namespace: "anything", want: true},
		{name: "single match", watched: []string{"a"}, namespace: "a", want: true},
		{name: "single miss", watched: []string{"a"}, namespace: "b", want: false},
		{name: "second of many", watched: []string{"a", "b"}, namespace: "b", want: true},
		{name: "not in many", watched: []string{"a", "b"}, namespace: "c", want: false},
		{name: "no prefix match", watched: []string{"app"}, namespace: "app-2", want: false},
		{name: "suffix glob", watched: []string{"team-*"}, namespace: "team-a", want: true},
		{name: "suffix glob miss", watched: []string{"team-*"}, namespace: "teams", want: false},
		{name: "prefix glob", watched: []string{"*-prod"}, namespace: "shop-prod", want: true},
		{name: "mixed exact and glob", watched: []string{"infra", "team-*"}, namespace: "infra", want: true},
		{name: "match all", watched: []string{"*"}, namespace: "anything", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NamespaceFilter(tt.watched).Generic(event.GenericEvent{
				Object: namespacedIngress(tt.namespace, "ing"),
			})
			if got != tt.want {
				t.Errorf("NamespaceFilter(%v) on %q = %v, want %v", tt.watched, tt.namespace, got, tt.want)
			}
		})
	}
}

func TestEnqueueAllIngressesRespectsWatchNamespaces(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add networkingv1 to scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		namespacedIngress("a", "one"),
		namespacedIngress("b", "two"),
		namespacedIngress("c", "three"),
	).Build()

	tests := []struct {
		name    string
		watched []string
		want    []string
	}{
		{name: "all namespaces", watched: nil, want: []string{"a", "b", "c"}},
		{name: "subset", watched: []string{"a", "c"}, want: []string{"a", "c"}},
		{name: "glob", watched: []string{"[ab]"}, want: []string{"a", "b"}},
		{name: "unknown namespace", watched: []string{"missing"}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &IngressReconciler{
				Client:              c,
				IngressClassFilters: []string{"*"},
				WatchNamespaces:     tt.watched,
			}
			var got []string
			for _, req := range r.enqueueAllIngresses(context.Background()) {
				got = append(got, req.Namespace)
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Errorf("enqueued namespaces = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIgnoredNamespaceFilter(t *testing.T) {
	tests := []struct {
		name      string
		ignored   []string
		namespace string
		want      bool
	}{
		{name: "no patterns ignores nothing", ignored: nil, namespace: "anything", want: true},
		{name: "exact match is dropped", ignored: []string{"kube-system"}, namespace: "kube-system", want: false},
		{name: "glob match is dropped", ignored: []string{"review-*"}, namespace: "review-mr-1", want: false},
		{name: "glob miss is kept", ignored: []string{"review-*"}, namespace: "reviews", want: true},
		{name: "any of many", ignored: []string{"a", "kube-*"}, namespace: "kube-public", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IgnoredNamespaceFilter(tt.ignored).Generic(event.GenericEvent{
				Object: namespacedIngress(tt.namespace, "ing"),
			})
			if got != tt.want {
				t.Errorf("IgnoredNamespaceFilter(%v) on %q = %v, want %v", tt.ignored, tt.namespace, got, tt.want)
			}
		})
	}
}

func TestNamespaceTranslated_IgnoreWinsOverWatch(t *testing.T) {
	tests := []struct {
		watched, ignored []string
		namespace        string
		want             bool
	}{
		{nil, nil, "app", true},
		{nil, []string{"review-*"}, "review-1", false},
		{[]string{"review-*"}, []string{"review-*"}, "review-1", false},
		{[]string{"team-*"}, []string{"team-legacy"}, "team-legacy", false},
		{[]string{"team-*"}, []string{"team-legacy"}, "team-new", true},
		{[]string{"team-*"}, nil, "other", false},
	}
	for _, tt := range tests {
		if got := namespaceTranslated(tt.watched, tt.ignored, tt.namespace); got != tt.want {
			t.Errorf("namespaceTranslated(%v, %v, %q) = %v, want %v", tt.watched, tt.ignored, tt.namespace, got, tt.want)
		}
	}
}

func TestEnqueueAllIngressesRespectsIgnoreNamespaces(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add networkingv1 to scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		namespacedIngress("app", "one"),
		namespacedIngress("review-1", "two"),
		namespacedIngress("review-2", "three"),
	).Build()
	r := &IngressReconciler{Client: c, IngressClassFilters: []string{"*"}, IgnoreNamespaces: []string{"review-*"}}

	requests := r.enqueueAllIngresses(context.Background())
	got := make([]string, 0, len(requests))
	for _, req := range requests {
		got = append(got, req.Namespace)
	}

	if !slices.Equal(got, []string{"app"}) {
		t.Errorf("enqueued namespaces = %v, want [app]", got)
	}
}
