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

func TestParseWatchNamespaces(t *testing.T) {
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
		got, err := ParseWatchNamespaces(tt.value)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseWatchNamespaces(%q) = %v, want error", tt.value, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseWatchNamespaces(%q) unexpected error: %v", tt.value, err)
			continue
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("ParseWatchNamespaces(%q) = %v, want %v", tt.value, got, tt.want)
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
