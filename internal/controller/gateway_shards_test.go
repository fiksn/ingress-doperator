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
	"fmt"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/translator"
)

// fillerHosts returns n distinct hostnames that no test route uses.
func fillerHosts(prefix string, n int) []string {
	hosts := make([]string, n)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("%s-%d.filler.example.com", prefix, i)
	}
	return hosts
}

func hostnames(hosts ...string) []gatewayv1.Hostname {
	out := make([]gatewayv1.Hostname, len(hosts))
	for i, host := range hosts {
		out[i] = gatewayv1.Hostname(host)
	}
	return out
}

func managedShard(name string, index int, hosts ...string) gatewayShard {
	return gatewayShard{name: name, index: index, gateway: testGateway(name, true, hosts...), managed: true}
}

func TestGatewayShardIndex(t *testing.T) {
	tests := []struct {
		name string
		want int
	}{
		{"nginx", 1},
		{"nginx-2", 2},
		{"nginx-17", 17},
		{"nginx-1", 0},
		{"nginx-02", 0},
		{"nginx-x", 0},
		{"nginx-public", 0},
		{"nginxx", 0},
		{"other-2", 0},
	}
	for _, tt := range tests {
		if got := gatewayShardIndex("nginx", tt.name); got != tt.want {
			t.Errorf("gatewayShardIndex(nginx, %q) = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestSelectGatewayShard(t *testing.T) {
	full := managedShard("nginx", 1, fillerHosts("a", MaxGatewayListeners)...)
	almostFull := managedShard("nginx", 1, fillerHosts("a", MaxGatewayListeners-1)...)
	second := managedShard("nginx-2", 2, "web.example.com")
	unmanaged := gatewayShard{name: "nginx-2", index: 2, gateway: testGateway("nginx-2", false)}

	tests := []struct {
		name    string
		shards  []gatewayShard
		current *gatewayShard
		hosts   []string
		want    string
	}{
		{"no shards uses base", nil, nil, []string{"web.example.com"}, "nginx"},
		{"base with room", []gatewayShard{almostFull}, nil, []string{"web.example.com"}, "nginx"},
		{"base full spills to new shard", []gatewayShard{full}, nil, []string{"web.example.com"}, "nginx-2"},
		{"route needing two listeners skips almost full base",
			[]gatewayShard{almostFull}, nil, []string{"x.example.com", "y.example.com"}, "nginx-2"},
		{"hostname already served stays on its shard",
			[]gatewayShard{almostFull, second}, nil, []string{"web.example.com"}, "nginx-2"},
		{"current shard is kept while it fits",
			[]gatewayShard{almostFull, second}, &second, []string{"new.example.com"}, "nginx-2"},
		{"current full shard is left for one with room",
			[]gatewayShard{almostFull, managedShard("nginx-2", 2, fillerHosts("b", MaxGatewayListeners)...)},
			&gatewayShard{name: "nginx-2", index: 2, managed: true,
				gateway: testGateway("nginx-2", true, fillerHosts("b", MaxGatewayListeners)...)},
			[]string{"new.example.com"}, "nginx"},
		{"current full shard serving a hostname is kept when another shard serves the rest",
			[]gatewayShard{managedShard("nginx", 1, "shared.example.com"), managedShard("nginx-2", 2,
				append(fillerHosts("b", MaxGatewayListeners-1), "web.example.com")...)},
			&gatewayShard{name: "nginx-2", index: 2, managed: true, gateway: testGateway("nginx-2", true,
				append(fillerHosts("b", MaxGatewayListeners-1), "web.example.com")...)},
			[]string{"web.example.com", "shared.example.com"}, "nginx-2"},
		{"full shard serving the hostname is kept",
			[]gatewayShard{managedShard("nginx", 1,
				append(fillerHosts("a", MaxGatewayListeners-1), "web.example.com")...)},
			nil, []string{"web.example.com", "new.example.com"}, "nginx"},
		{"unmanaged shard is skipped", []gatewayShard{full, unmanaged}, nil, []string{"web.example.com"}, "nginx-3"},
		{"free index between shards is reused",
			[]gatewayShard{full, managedShard("nginx-3", 3, fillerHosts("c", MaxGatewayListeners)...)},
			nil, []string{"web.example.com"}, "nginx-2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectGatewayShard("nginx", tt.shards, tt.current, hostnames(tt.hosts...)); got != tt.want {
				t.Errorf("selectGatewayShard() = %q, want %q", got, tt.want)
			}
		})
	}
}

func newShardingReconciler(t *testing.T, objs ...client.Object) *IngressReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add networkingv1 to scheme: %v", err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to add gatewayv1 to scheme: %v", err)
	}
	return &IngressReconciler{
		Client:           fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		GatewayNamespace: testGatewayNamespace,
		GatewaySharding:  true,
	}
}

func TestApplyGatewaySharding(t *testing.T) {
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}}
	base := testGateway("nginx", true, fillerHosts("a", MaxGatewayListeners)...)
	otherShard := testGateway("nginx-2", true, fillerHosts("b", MaxGatewayListeners-1)...)
	existing := testRoute("web", "nginx-2", "nginx-2", "web.example.com")

	tests := []struct {
		name     string
		sharding bool
		dedicate bool
		objs     []client.Object
		want     string
	}{
		{"sharding off keeps base", false, false, []client.Object{base}, "nginx"},
		{"full base spills over", true, false, []client.Object{base}, "nginx-2"},
		{"dedicated Gateway is not sharded", true, true, []client.Object{base}, "nginx"},
		{"existing route keeps its shard", true, false, []client.Object{base, otherShard, existing}, "nginx-2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newShardingReconciler(t, tt.objs...)
			r.GatewaySharding = tt.sharding
			if tt.dedicate {
				r.OneGatewayPerIngress = []string{"app/web"}
			}
			route := testRoute("web", "nginx", "", "web.example.com")

			got, err := r.applyGatewaySharding(context.Background(), ingress, "nginx", []*gatewayv1.HTTPRoute{route})
			if err != nil {
				t.Fatalf("applyGatewaySharding() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("applyGatewaySharding() = %q, want %q", got, tt.want)
			}
			if parent := string(route.Spec.ParentRefs[0].Name); parent != tt.want {
				t.Errorf("parentRef = %q, want %q", parent, tt.want)
			}
		})
	}
}

func TestUpdateGatewayListeners_StopsAtListenerLimit(t *testing.T) {
	gw := testGateway("nginx", true, fillerHosts("a", MaxGatewayListeners-1)...)
	route := testRoute("web", "nginx", "", "a.example.com")
	route.Spec.Hostnames = hostnames("a.example.com", "b.example.com", "c.example.com")
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}}
	r := &HTTPRouteReconciler{}

	updated, dropped := r.updateGatewayListeners(context.Background(), gw, route, ingress)

	if !updated {
		t.Error("updated = false, want true for the listener that fits")
	}
	if len(gw.Spec.Listeners) != MaxGatewayListeners {
		t.Errorf("listeners = %d, want %d", len(gw.Spec.Listeners), MaxGatewayListeners)
	}
	if len(dropped) != 2 || dropped[0] != "b.example.com" || dropped[1] != "c.example.com" {
		t.Errorf("dropped = %v, want [b.example.com c.example.com]", dropped)
	}
}

func TestReconcileGatewayListeners_StopsAtListenerLimit(t *testing.T) {
	existing := fillerHosts("a", MaxGatewayListeners-1)
	gw := testGateway("nginx", true, existing...)
	routes := make([]gatewayv1.HTTPRoute, 0, len(existing)+2)
	for i, host := range append(existing, "y.example.com", "z.example.com") {
		route := testRoute(fmt.Sprintf("r%d", i), "nginx", "nginx", host)
		route.Annotations[translator.SourceAnnotation] = "app/web"
		routes = append(routes, *route)
	}
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}}
	r := newReleaseReconciler(t, gw, ingress)

	if _, err := r.reconcileGatewayListeners(context.Background(), "nginx", routes); err != nil {
		t.Fatalf("reconcileGatewayListeners() error: %v", err)
	}

	got, _ := getGateway(t, r, "nginx")
	names := listenerNames(got)
	if len(names) != MaxGatewayListeners {
		t.Errorf("listeners = %d, want %d", len(names), MaxGatewayListeners)
	}
	if !names["y.example.com"] || names["z.example.com"] {
		t.Errorf("want y.example.com added and z.example.com left out (sorted order), got %v", names)
	}
}
