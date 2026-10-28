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

package utils

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func managedRoute(annotations map[string]string) *gatewayv1.HTTPRoute {
	all := map[string]string{ManagedByAnnotation: ManagedByValue}
	for k, v := range annotations {
		all[k] = v
	}
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app", Annotations: all},
	}
}

func TestApplyHTTPRoute_KeepsListenerGatewayAnnotation(t *testing.T) {
	tests := []struct {
		name     string
		existing map[string]string
		want     string
		wantSet  bool
	}{
		{"preserved across update", map[string]string{ListenerGatewayAnnotation: "nginx"}, "nginx", true},
		{"not invented when absent", nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := gatewayv1.Install(scheme); err != nil {
				t.Fatalf("failed to add gatewayv1 to scheme: %v", err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(managedRoute(tt.existing)).Build()
			m := &HTTPRouteManager{Client: c}

			desired := managedRoute(map[string]string{"example.com/new": "value"})
			if err := m.applyHTTPRoute(context.Background(), desired, nil); err != nil {
				t.Fatalf("applyHTTPRoute() error: %v", err)
			}

			got := &gatewayv1.HTTPRoute{}
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "api"}, got); err != nil {
				t.Fatalf("failed to get route: %v", err)
			}
			value, ok := got.Annotations[ListenerGatewayAnnotation]
			if ok != tt.wantSet || value != tt.want {
				t.Errorf("listener-gateway annotation = %q (set=%v), want %q (set=%v)", value, ok, tt.want, tt.wantSet)
			}
			if got.Annotations["example.com/new"] != "value" {
				t.Errorf("desired annotations not applied: %v", got.Annotations)
			}
			if _, ok := desired.Annotations[ListenerGatewayAnnotation]; ok {
				t.Error("applyHTTPRoute must not mutate the desired route's annotations")
			}
		})
	}
}
