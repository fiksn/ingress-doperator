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
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// listenersMissingCapacity returns the hostnames of the routes that would get no listener on the
// Gateway because it has reached MaxGatewayListeners. The listener update is simulated on a copy of
// the Gateway with the same logic used to write it, so wildcard listeners and listeners other
// controllers own are taken into account. A Gateway ingress-doperator does not manage is not
// checked: its listeners are never written anyway.
func (r *IngressReconciler) listenersMissingCapacity(
	ctx context.Context,
	gatewayName string,
	ingress *networkingv1.Ingress,
	routes []*gatewayv1.HTTPRoute,
) ([]string, *gatewayv1.Gateway, error) {
	gateway, canManage, _, err := r.ensureGatewayForListenerUpdate(ctx, gatewayName, ingress)
	if err != nil || !canManage {
		return nil, gateway, err
	}
	simulated := gateway.DeepCopy()
	listenerReconciler := r.newGatewayListenerReconciler()
	var dropped []string
	for _, route := range routes {
		_, routeDropped := listenerReconciler.updateGatewayListeners(ctx, simulated, route, ingress)
		dropped = append(dropped, routeDropped...)
	}
	return dropped, gateway, nil
}

// skipForFullGateway reports whether the Ingress's HTTPRoutes must not be applied because the
// Gateway cannot take all the listeners they need. Applying them would point hostnames at listeners
// that do not exist, so HTTPS for those hosts stops working; the existing HTTPRoutes are kept
// instead, and the Ingress is retried until listeners are freed or the Gateway configuration changes.
func (r *IngressReconciler) skipForFullGateway(
	ctx context.Context,
	gatewayName string,
	ingress *networkingv1.Ingress,
	routes []*gatewayv1.HTTPRoute,
) (bool, error) {
	dropped, gateway, err := r.listenersMissingCapacity(ctx, gatewayName, ingress, routes)
	if err != nil || len(dropped) == 0 {
		return false, err
	}
	log.FromContext(ctx).Info("Not updating HTTPRoutes: Gateway has no room for their listeners",
		"namespace", ingress.Namespace, "name", ingress.Name,
		"gateway", gateway.Namespace+"/"+gateway.Name, "hostnames", dropped)
	r.recordWarning(ingress, "GatewayListenerLimit", fmt.Sprintf(
		"Gateway %s/%s has reached the limit of %d listeners; no listener for: %s. The HTTPRoutes are not "+
			"created or updated until there is room. Enable --gateway-sharding or move Ingresses to other Gateways.",
		gateway.Namespace, gateway.Name, MaxGatewayListeners, strings.Join(dropped, ", ")))
	return true, nil
}
