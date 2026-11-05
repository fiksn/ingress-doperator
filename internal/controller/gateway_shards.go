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
	"sort"
	"strconv"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/translator"
)

// gatewayShard is a shared Gateway named <base> (index 1) or <base>-<index> (index >= 2). gateway
// is nil for a shard that does not exist yet; an existing Gateway not managed by ingress-doperator
// occupies its index but takes no listeners.
type gatewayShard struct {
	name    string
	index   int
	gateway *gatewayv1.Gateway
	managed bool
}

// gatewayShardName returns the name of shard index of the shared Gateway base.
func gatewayShardName(base string, index int) string {
	if index <= 1 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, index)
}

// gatewayShardIndex returns the shard index of name for the shared Gateway base, or 0 if name is
// not one of its shards.
func gatewayShardIndex(base, name string) int {
	if name == base {
		return 1
	}
	suffix, ok := strings.CutPrefix(name, base+"-")
	if !ok {
		return 0
	}
	index, err := strconv.Atoi(suffix)
	if err != nil || index < 2 || strconv.Itoa(index) != suffix {
		return 0
	}
	return index
}

// newListenersNeeded counts the hostnames that have no ingress-doperator listener on gateway yet.
func newListenersNeeded(gateway *gatewayv1.Gateway, hostnames []gatewayv1.Hostname) int {
	needed := 0
	for _, hostname := range hostnames {
		if idx, _ := findListenerForHostname(gateway, string(hostname)); idx < 0 {
			needed++
		}
	}
	return needed
}

// fits reports whether all hostnames can be served by the shard without exceeding the listener
// limit. A shard whose Gateway does not exist yet fits any route within the limit.
func (s gatewayShard) fits(hostnames []gatewayv1.Hostname) bool {
	if !s.managed {
		return false
	}
	if s.gateway == nil {
		return len(hostnames) <= MaxGatewayListeners
	}
	return len(s.gateway.Spec.Listeners)+newListenersNeeded(s.gateway, hostnames) <= MaxGatewayListeners
}

// serves reports whether the shard already has a listener for any of hostnames.
func (s gatewayShard) serves(hostnames []gatewayv1.Hostname) bool {
	return s.managed && s.gateway != nil && newListenersNeeded(s.gateway, hostnames) < len(hostnames)
}

// selectGatewayShard picks the shard for a route with hostnames, currently attached to current
// (nil if it has no shard yet). Hostnames keep their Gateway (and load balancer address): a route
// whose current shard already serves any of its hostnames stays there even when new hostnames do
// not fit, which then stay without a listener. Next, shards already serving one of the hostnames
// (from other routes) come first, then the current shard if it fits, then the first shard with
// room, then a new one. When shards serving the hostnames are all full, the first of them is used.
func selectGatewayShard(
	base string,
	shards []gatewayShard,
	current *gatewayShard,
	hostnames []gatewayv1.Hostname,
) string {
	if current != nil && current.serves(hostnames) {
		return current.name
	}
	serving := servingShards(shards, hostnames)
	if current != nil && current.fits(hostnames) && len(serving) == 0 {
		return current.name
	}
	if len(serving) > 0 {
		return firstFittingShard(serving, hostnames, serving[0].name)
	}
	return firstFittingShard(shards, hostnames, gatewayShardName(base, nextFreeShardIndex(shards)))
}

func servingShards(shards []gatewayShard, hostnames []gatewayv1.Hostname) []gatewayShard {
	var serving []gatewayShard
	for _, shard := range shards {
		if shard.serves(hostnames) {
			serving = append(serving, shard)
		}
	}
	return serving
}

// firstFittingShard returns the first shard with room for hostnames, or fallback.
func firstFittingShard(shards []gatewayShard, hostnames []gatewayv1.Hostname, fallback string) string {
	for _, shard := range shards {
		if shard.fits(hostnames) {
			return shard.name
		}
	}
	return fallback
}

// nextFreeShardIndex returns the lowest shard index without a Gateway; shards is sorted by index.
func nextFreeShardIndex(shards []gatewayShard) int {
	index := 1
	for _, shard := range shards {
		if shard.index != index {
			break
		}
		index++
	}
	return index
}

// listGatewayShards returns the existing shards of the shared Gateway base, sorted by index.
func (r *IngressReconciler) listGatewayShards(ctx context.Context, base string) ([]gatewayShard, error) {
	gateways := &gatewayv1.GatewayList{}
	if err := r.List(ctx, gateways, client.InNamespace(r.GatewayNamespace)); err != nil {
		return nil, fmt.Errorf("failed to list Gateways in %s for sharding: %w", r.GatewayNamespace, err)
	}
	var shards []gatewayShard
	for i := range gateways.Items {
		gateway := &gateways.Items[i]
		index := gatewayShardIndex(base, gateway.Name)
		if index == 0 {
			continue
		}
		managed := gateway.Annotations[translator.ManagedByAnnotation] == translator.ManagedByValue
		shards = append(shards, gatewayShard{name: gateway.Name, index: index, gateway: gateway, managed: managed})
	}
	sort.Slice(shards, func(i, j int) bool { return shards[i].index < shards[j].index })
	return shards, nil
}

// currentGatewayShard returns the shard the Ingress's existing HTTPRoute (or its plain HTTP route)
// is attached to, or nil.
func (r *IngressReconciler) currentGatewayShard(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	base string,
	shards []gatewayShard,
) (*gatewayShard, error) {
	route, err := r.existingIngressRoute(ctx, ingress)
	if err != nil || route == nil {
		return nil, err
	}
	name := parentGatewayName(route, r.GatewayNamespace)
	index := gatewayShardIndex(base, name)
	if index == 0 {
		return nil, nil
	}
	for i := range shards {
		if shards[i].index == index {
			return &shards[i], nil
		}
	}
	return &gatewayShard{name: name, index: index, managed: true}, nil
}

// existingIngressRoute returns the Ingress's HTTPS HTTPRoute, or its plain HTTP one, or nil.
func (r *IngressReconciler) existingIngressRoute(
	ctx context.Context,
	ingress *networkingv1.Ingress,
) (*gatewayv1.HTTPRoute, error) {
	for _, name := range []string{ingress.Name, ingress.Name + httpRouteSuffix} {
		route := &gatewayv1.HTTPRoute{}
		key := types.NamespacedName{Namespace: ingress.Namespace, Name: name}
		err := r.Get(ctx, key, route)
		if err == nil {
			return route, nil
		}
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get HTTPRoute %s for sharding: %w", key, err)
		}
	}
	return nil, nil
}

// parentGatewayName returns the name of the first parent Gateway of httpRoute in gatewayNamespace.
func parentGatewayName(httpRoute *gatewayv1.HTTPRoute, gatewayNamespace string) string {
	for _, parentRef := range httpRoute.Spec.ParentRefs {
		if parentRef.Namespace != nil && string(*parentRef.Namespace) == gatewayNamespace {
			return string(parentRef.Name)
		}
	}
	return ""
}

// gatewayShardFor returns the shard of the shared Gateway base that the Ingress's HTTPRoute should
// attach to (--gateway-sharding); hostnames are the hostnames of the listeners the route needs.
func (r *IngressReconciler) gatewayShardFor(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	base string,
	hostnames []gatewayv1.Hostname,
) (string, error) {
	shards, err := r.listGatewayShards(ctx, base)
	if err != nil {
		return "", err
	}
	current, err := r.currentGatewayShard(ctx, ingress, base, shards)
	if err != nil {
		return "", err
	}
	return selectGatewayShard(base, shards, current, hostnames), nil
}

// applyGatewaySharding attaches the Ingress's HTTPRoutes to one shard of the shared Gateway
// gatewayName when --gateway-sharding is on and returns the Gateway they attach to. Only the
// hostnames of HTTPS routes need listeners of their own; plain HTTP routes share the catch-all
// port 80 listener. Dedicated Gateways (--one-gateway-per-ingress) are never sharded.
func (r *IngressReconciler) applyGatewaySharding(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	gatewayName string,
	httpRoutes []*gatewayv1.HTTPRoute,
) (string, error) {
	if !r.GatewaySharding || r.wantsDedicatedGateway(ingress) {
		return gatewayName, nil
	}
	var hostnames []gatewayv1.Hostname
	for _, httpRoute := range httpRoutes {
		if !attachesToHTTPListener(httpRoute) {
			hostnames = append(hostnames, listenerHostnames(r.WildcardListeners, httpRoute.Spec.Hostnames)...)
		}
	}
	shard, err := r.gatewayShardFor(ctx, ingress, gatewayName, hostnames)
	if err != nil {
		return "", err
	}
	for _, httpRoute := range httpRoutes {
		setParentGateway(httpRoute, shard)
	}
	return shard, nil
}

// setParentGateway points every parentRef of httpRoute at the Gateway name.
func setParentGateway(httpRoute *gatewayv1.HTTPRoute, name string) {
	for i := range httpRoute.Spec.ParentRefs {
		httpRoute.Spec.ParentRefs[i].Name = gatewayv1.ObjectName(name)
	}
}
