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

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func tcpListener(name string) gatewayv1.Listener {
	return gatewayv1.Listener{Name: gatewayv1.SectionName(name), Port: 5432, Protocol: gatewayv1.TCPProtocolType}
}

func listenerNames(gw *gatewayv1.Gateway) map[string]bool {
	names := make(map[string]bool, len(gw.Spec.Listeners))
	for _, l := range gw.Spec.Listeners {
		names[string(l.Name)] = true
	}
	return names
}

func TestReconcileGatewayListeners_KeepsForeignListeners(t *testing.T) {
	gw := testGateway("app-api", true, "stale.example.com", "web.example.com")
	gw.Spec.Listeners = append(gw.Spec.Listeners, tcpListener("postgres"))
	r := newReleaseReconciler(t, gw, testRoute("web", "app-api", "app-api", "web.example.com"))
	routes := []gatewayv1.HTTPRoute{*testRoute("web", "app-api", "app-api", "web.example.com")}

	if _, err := r.reconcileGatewayListeners(context.Background(), "app-api", routes); err != nil {
		t.Fatalf("reconcileGatewayListeners() error: %v", err)
	}

	got, _ := getGateway(t, r, "app-api")
	names := listenerNames(got)
	if !names["postgres"] {
		t.Error("TCP listener added by someone else must be kept")
	}
	if names["stale.example.com"] {
		t.Error("our listener without routes should be removed")
	}
	if !names["web.example.com"] {
		t.Error("our listener with a route should be kept")
	}
}

func TestReconcileGatewayListeners_KeepsGatewayWithOnlyForeignListeners(t *testing.T) {
	gw := testGateway("app-api", true, "api.example.com")
	gw.Spec.Listeners = append(gw.Spec.Listeners, tcpListener("postgres"))
	r := newReleaseReconciler(t, gw)

	if _, err := r.reconcileGatewayListeners(context.Background(), "app-api", nil); err != nil {
		t.Fatalf("reconcileGatewayListeners() error: %v", err)
	}

	got, ok := getGateway(t, r, "app-api")
	if !ok {
		t.Fatal("Gateway with a foreign listener must not be deleted when its HTTPRoutes are gone")
	}
	if names := listenerNames(got); len(names) != 1 || !names["postgres"] {
		t.Errorf("listeners = %v, want only postgres", names)
	}
}

func TestFindListenerForHostname(t *testing.T) {
	const host = "api.example.com"
	ours := testGateway("gw", true, host).Spec.Listeners[0]
	tests := []struct {
		name        string
		listeners   []gatewayv1.Listener
		wantIdx     int
		wantBlocked bool
	}{
		{"no listeners", nil, -1, false},
		{"our listener", []gatewayv1.Listener{tcpListener("db"), ours}, 1, false},
		{"foreign TCP listener is unrelated", []gatewayv1.Listener{tcpListener("db")}, -1, false},
		{"foreign HTTP listener on port 80 does not block HTTPS", []gatewayv1.Listener{{
			Name: "plain", Hostname: ptr.To(gatewayv1.Hostname(host)), Port: 80, Protocol: gatewayv1.HTTPProtocolType,
		}}, -1, false},
		{"foreign HTTPS listener for the hostname blocks", []gatewayv1.Listener{{
			Name: "custom", Hostname: ptr.To(gatewayv1.Hostname(host)), Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
		}}, -1, true},
		{"foreign listener with our name blocks", []gatewayv1.Listener{tcpListener(host)}, -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := &gatewayv1.Gateway{Spec: gatewayv1.GatewaySpec{Listeners: tt.listeners}}
			idx, blocked := findListenerForHostname(gw, host)
			if idx != tt.wantIdx || blocked != tt.wantBlocked {
				t.Errorf("findListenerForHostname() = (%d, %v), want (%d, %v)", idx, blocked, tt.wantIdx, tt.wantBlocked)
			}
		})
	}
}

func TestUpdateGatewayListeners_DoesNotTouchForeignListenerForHostname(t *testing.T) {
	foreign := gatewayv1.Listener{
		Name:     "custom",
		Hostname: ptr.To(gatewayv1.Hostname("api.example.com")),
		Port:     443,
		Protocol: gatewayv1.HTTPSProtocolType,
	}
	gw := &gatewayv1.Gateway{Spec: gatewayv1.GatewaySpec{Listeners: []gatewayv1.Listener{foreign}}}
	route := testRoute("api", "gw", "", "api.example.com")
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "api"}}
	r := &HTTPRouteReconciler{}

	if r.updateGatewayListeners(context.Background(), gw, route, ingress) {
		t.Error("no change expected when a foreign listener serves the hostname")
	}
	if len(gw.Spec.Listeners) != 1 || gw.Spec.Listeners[0].AllowedRoutes != nil {
		t.Errorf("foreign listener must be left untouched, got %+v", gw.Spec.Listeners)
	}
}
