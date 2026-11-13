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

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/utils"
)

const gatewayKind = "Gateway"

// RouteAccepted reports whether every Gateway parent of the route has accepted its current
// generation. external-dns publishes a route's hostnames only through parents that accepted it,
// so an Ingress must not be hidden from external-dns before this holds. When it does not, reason
// names the first parent that has not accepted the route and why.
func RouteAccepted(route *gatewayv1.HTTPRoute) (accepted bool, reason string) {
	routeKey := route.Namespace + "/" + route.Name
	for _, ref := range route.Spec.ParentRefs {
		if !isGatewayParentRef(ref) {
			continue
		}
		parent := parentRefString(route.Namespace, ref)
		status, found := findParentStatus(route, ref)
		if !found {
			return false, fmt.Sprintf("HTTPRoute %s: Gateway %s has not reported a status", routeKey, parent)
		}
		accepted := findCondition(status.Conditions, string(gatewayv1.RouteConditionAccepted))
		switch {
		case accepted == nil:
			return false, fmt.Sprintf("HTTPRoute %s: Gateway %s has not reported Accepted", routeKey, parent)
		case accepted.ObservedGeneration != 0 && accepted.ObservedGeneration < route.Generation:
			return false, fmt.Sprintf("HTTPRoute %s: Gateway %s has not processed generation %d yet",
				routeKey, parent, route.Generation)
		case accepted.Status != metav1.ConditionTrue:
			return false, fmt.Sprintf("HTTPRoute %s: Gateway %s did not accept it (%s: %s)",
				routeKey, parent, accepted.Reason, accepted.Message)
		}
	}
	return true, ""
}

// IngressRoutesAccepted reports whether the Ingress has at least one HTTPRoute managed by
// ingress-doperator and every one of them is accepted (see RouteAccepted).
func IngressRoutesAccepted(
	ctx context.Context,
	cli client.Client,
	ingress *networkingv1.Ingress,
) (accepted bool, reason string, err error) {
	routes := &gatewayv1.HTTPRouteList{}
	if err := cli.List(ctx, routes, client.InNamespace(ingress.Namespace)); err != nil {
		return false, "", fmt.Errorf("failed to list HTTPRoutes of Ingress %s/%s: %w",
			ingress.Namespace, ingress.Name, err)
	}
	found := false
	for i := range routes.Items {
		route := &routes.Items[i]
		if !utils.IsManagedByUsForIngress(route, ingress.Namespace, ingress.Name) {
			continue
		}
		found = true
		if ok, why := RouteAccepted(route); !ok {
			return false, why, nil
		}
	}
	if !found {
		return false, fmt.Sprintf("Ingress %s/%s has no HTTPRoute yet", ingress.Namespace, ingress.Name), nil
	}
	return true, "", nil
}

func isGatewayParentRef(ref gatewayv1.ParentReference) bool {
	group := gatewayv1.GroupName
	if ref.Group != nil {
		group = string(*ref.Group)
	}
	kind := gatewayKind
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}
	return group == gatewayv1.GroupName && kind == gatewayKind
}

func findParentStatus(route *gatewayv1.HTTPRoute, ref gatewayv1.ParentReference) (gatewayv1.RouteParentStatus, bool) {
	want := parentRefString(route.Namespace, ref)
	for _, status := range route.Status.Parents {
		if parentRefString(route.Namespace, status.ParentRef) == want {
			return status, true
		}
	}
	return gatewayv1.RouteParentStatus{}, false
}

// parentRefString identifies a Gateway parent reference by namespace, name, section and port,
// defaulting the namespace to the route's.
func parentRefString(routeNamespace string, ref gatewayv1.ParentReference) string {
	namespace := routeNamespace
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}
	key := namespace + "/" + string(ref.Name)
	if ref.SectionName != nil {
		key += " section " + string(*ref.SectionName)
	}
	if ref.Port != nil {
		key += fmt.Sprintf(" port %d", *ref.Port)
	}
	return key
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
