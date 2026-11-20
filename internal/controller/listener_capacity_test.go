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
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/translator"
	"github.com/fiksn/ingress-doperator/internal/utils"
)

// staleRouteOnFullGateway is an Ingress whose HTTPRoute was created on Gateway "private" while it had
// room; the Gateway has since been filled with other hostnames and has no listener for this one.
func staleRouteOnFullGateway(t *testing.T) (*networkingv1.Ingress, *gatewayv1.HTTPRoute, *gatewayv1.Gateway) {
	t.Helper()
	ingress := multiHostIngress(hostRule(consoleHost, ingressPath("/", networkingv1.PathTypePrefix, "web", numberedPort(80))))
	full := testGateway("private", true, fillerHosts("a", MaxGatewayListeners)...)
	full.Namespace = testGatewayNamespace
	route := &gatewayv1.HTTPRoute{}
	route.Namespace, route.Name = ingress.Namespace, ingress.Name
	route.Annotations = map[string]string{
		translator.ManagedByAnnotation:  translator.ManagedByValue,
		translator.SourceAnnotation:     ingress.Namespace + "/" + ingress.Name,
		utils.ListenerGatewayAnnotation: "private",
	}
	route.Spec.Hostnames = []gatewayv1.Hostname{consoleHost}
	route.Spec.ParentRefs = []gatewayv1.ParentReference{{
		Name:        "private",
		Namespace:   ptr.To(gatewayv1.Namespace(testGatewayNamespace)),
		SectionName: ptr.To(gatewayv1.SectionName(consoleHost)),
	}}
	route.Spec.Rules = []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{
		BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
			Name: "old-backend", Port: ptr.To(gatewayv1.PortNumber(80)),
		}},
	}}}}
	return ingress, route, full
}

func parentGateways(route *gatewayv1.HTTPRoute) string {
	names := make([]string, 0, len(route.Spec.ParentRefs))
	for _, ref := range route.Spec.ParentRefs {
		names = append(names, string(ref.Name))
	}
	return strings.Join(names, ",")
}

// TestFullGateway_ShardingMovesStaleRoute: with --gateway-sharding the route moves to a shard
// with room, which gets the listener.
func TestFullGateway_ShardingMovesStaleRoute(t *testing.T) {
	ingress, route, full := staleRouteOnFullGateway(t)
	r := newTranslationReconciler(t, ingress, route, full)
	r.GatewaySharding = true

	reconcileIngress(t, r, ingress)

	routes := ingressRoutes(t, r, ingress)
	if got := parentGateways(&routes[0]); got != "private-2" {
		t.Fatalf("route parent Gateways = %s, want private-2", got)
	}
	shard := &gatewayv1.Gateway{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: testGatewayNamespace, Name: "private-2"}, shard); err != nil {
		t.Fatalf("shard private-2 not created: %v", err)
	}
	if idx, _ := findListenerForHostname(shard, consoleHost); idx < 0 {
		t.Errorf("shard private-2 has no listener for %s", consoleHost)
	}
}

// TestFullGateway_WithoutShardingKeepsRoute: without sharding the route is not updated (its
// hostname would point at a listener that cannot be created) and a warning is recorded.
func TestFullGateway_WithoutShardingKeepsRoute(t *testing.T) {
	ingress, route, full := staleRouteOnFullGateway(t)
	recorder := events.NewFakeRecorder(10)
	r := newTranslationReconciler(t, ingress, route, full)
	r.Recorder = recorder

	result := reconcileIngress(t, r, ingress)

	if result.RequeueAfter == 0 {
		t.Error("Reconcile() did not requeue")
	}
	routes := ingressRoutes(t, r, ingress)
	if got := string(routes[0].Spec.Rules[0].BackendRefs[0].Name); got != "old-backend" {
		t.Errorf("route was updated (backend %s) although its listener cannot be added", got)
	}
	if got := drainEvents(recorder); !strings.Contains(got, "Warning GatewayListenerLimit") || !strings.Contains(got, consoleHost) {
		t.Errorf("no GatewayListenerLimit warning naming %s:\n%s", consoleHost, got)
	}
}

// TestGatewayShardAnnotation_PinsRoute moves an Ingress to a shard by annotation, even without
// --gateway-sharding and with room on the base Gateway.
func TestGatewayShardAnnotation_PinsRoute(t *testing.T) {
	ingress := multiHostIngress(hostRule(consoleHost, ingressPath("/", networkingv1.PathTypePrefix, "web", numberedPort(80))))
	ingress.Annotations = map[string]string{GatewayShardAnnotation: "private-2"}
	r := newTranslationReconciler(t, ingress)

	reconcileIngress(t, r, ingress)

	routes := ingressRoutes(t, r, ingress)
	if got := parentGateways(&routes[0]); got != "private-2" {
		t.Fatalf("route parent Gateways = %s, want private-2", got)
	}
	shard := &gatewayv1.Gateway{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: testGatewayNamespace, Name: "private-2"}, shard); err != nil {
		t.Fatalf("pinned shard private-2 not created: %v", err)
	}
}

func TestGatewayShardAnnotation_OtherGatewayIgnored(t *testing.T) {
	ingress := multiHostIngress(hostRule(consoleHost, ingressPath("/", networkingv1.PathTypePrefix, "web", numberedPort(80))))
	ingress.Annotations = map[string]string{GatewayShardAnnotation: "public-2"}
	recorder := events.NewFakeRecorder(10)
	r := newTranslationReconciler(t, ingress)
	r.Recorder = recorder

	reconcileIngress(t, r, ingress)

	routes := ingressRoutes(t, r, ingress)
	if got := parentGateways(&routes[0]); got != "private" {
		t.Errorf("route parent Gateways = %s, want the class Gateway private", got)
	}
	if got := drainEvents(recorder); !strings.Contains(got, "Warning GatewayShardIgnored") || !strings.Contains(got, "public-2") {
		t.Errorf("no GatewayShardIgnored warning:\n%s", got)
	}
}
