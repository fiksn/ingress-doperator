/*
Copyright 2026.

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

package main

import (
	"context"
	"fmt"
	"maps"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/controller"
	"github.com/fiksn/ingress-doperator/internal/utils"
)

// dnsTarget is the resource external-dns should publish an Ingress's hostnames from (--dns-target).
type dnsTarget string

const (
	dnsTargetIngress   dnsTarget = "ingress"
	dnsTargetHTTPRoute dnsTarget = "httproute"

	annotationTrue = "true"
)

func parseDNSTarget(value string) (dnsTarget, error) {
	switch target := dnsTarget(value); target {
	case "", dnsTargetIngress, dnsTargetHTTPRoute:
		return target, nil
	default:
		return "", fmt.Errorf("--dns-target must be %q or %q, got %q", dnsTargetIngress, dnsTargetHTTPRoute, value)
	}
}

// switchDNSTarget moves the Ingress's DNS records to opts.dnsTarget. It sets the per-Ingress
// annotations that override the operator's --ingress-postprocessing mode, so the operator keeps
// the result, and applies the change itself so it also takes effect while the operator is stopped.
func switchDNSTarget(
	ctx context.Context,
	cli client.Client,
	manager *utils.HTTPRouteManager,
	ingress *networkingv1.Ingress,
	opts reenablerOptions,
) error {
	if ingress.Annotations[controller.IngressDisabledAnnotation] == controller.IngressDisabledReasonNormal {
		return fmt.Errorf("ingress is disabled by ingress-doperator; re-enable it with --restore-class first")
	}
	routes, err := managedRoutesForIngress(ctx, manager, ingress)
	if err != nil {
		return err
	}
	switch opts.dnsTarget {
	case dnsTargetIngress:
		err = moveDNSToIngress(ctx, cli, ingress, routes)
	case dnsTargetHTTPRoute:
		err = moveDNSToHTTPRoute(ctx, cli, ingress, routes)
	default:
		return fmt.Errorf("unknown dns target %q", opts.dnsTarget)
	}
	if err != nil {
		return err
	}
	setupLog.Info("Moved DNS records", "namespace", ingress.Namespace, "name", ingress.Name,
		"target", opts.dnsTarget, "httproutes", len(routes))
	return nil
}

// moveDNSToIngress restores the Ingress's external-dns annotations and pins it to
// shadow-external-dns in a single update, so the operator never sees it without an override,
// then shadows its HTTPRoutes. The Ingress publishes before the routes stop, so the record is
// never left without a source.
func moveDNSToIngress(
	ctx context.Context,
	cli client.Client,
	ingress *networkingv1.Ingress,
	routes []gatewayv1.HTTPRoute,
) error {
	err := updateIngressWithRetry(ctx, cli, ingress, func(updated *networkingv1.Ingress) (bool, error) {
		if updated.Annotations == nil {
			updated.Annotations = map[string]string{}
		}
		modified := restoreExternalDNSAnnotations(updated.Annotations)
		if updated.Annotations[controller.ShadowExternalDNSAnnotation] != annotationTrue {
			updated.Annotations[controller.ShadowExternalDNSAnnotation] = annotationTrue
			modified = true
		}
		return modified, nil
	})
	if err != nil {
		return fmt.Errorf("failed to restore external-dns on Ingress: %w", err)
	}
	return updateRoutes(ctx, cli, routes, controller.ShadowExternalDNS)
}

// moveDNSToHTTPRoute marks the Ingress as cut over, un-shadows its HTTPRoutes and only then
// hides the Ingress from external-dns, so the record is never left without a source.
func moveDNSToHTTPRoute(
	ctx context.Context,
	cli client.Client,
	ingress *networkingv1.Ingress,
	routes []gatewayv1.HTTPRoute,
) error {
	if ingress.Annotations[controller.IgnoreIngressAnnotation] == annotationTrue {
		return fmt.Errorf("ingress has %s=true, so the operator no longer updates its HTTPRoutes; "+
			"remove the annotation first", controller.IgnoreIngressAnnotation)
	}
	if len(routes) == 0 {
		return fmt.Errorf("no HTTPRoute managed by ingress-doperator exists for this Ingress yet; " +
			"wait for the operator to convert it")
	}
	err := updateIngressWithRetry(ctx, cli, ingress, func(updated *networkingv1.Ingress) (bool, error) {
		if updated.Annotations == nil {
			updated.Annotations = map[string]string{}
		}
		_, shadowed := updated.Annotations[controller.ShadowExternalDNSAnnotation]
		delete(updated.Annotations, controller.ShadowExternalDNSAnnotation)
		if updated.Annotations[controller.IngressDisabledAnnotation] == controller.IngressDisabledReasonExternalDNS {
			return shadowed, nil
		}
		updated.Annotations[controller.IngressDisabledAnnotation] = controller.IngressDisabledReasonExternalDNS
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("failed to mark Ingress as cut over: %w", err)
	}
	if err := updateRoutes(ctx, cli, routes, unshadowExternalDNS); err != nil {
		return err
	}
	if err := controller.DisableExternalDNS(ctx, cli, ingress); err != nil {
		return fmt.Errorf("failed to disable external-dns on Ingress: %w", err)
	}
	return nil
}

// unshadowExternalDNS undoes controller.ShadowExternalDNS so external-dns publishes the route's
// hostnames. The operator restores the remaining external-dns annotations from the Ingress on its
// next reconcile.
func unshadowExternalDNS(route *gatewayv1.HTTPRoute) {
	source := controller.ExternalDNSGatewayHostnameSource
	if route.Annotations[source] == controller.ExternalDNSHostnameSourceAnnotationOnly {
		delete(route.Annotations, source)
	}
}

func updateRoutes(
	ctx context.Context,
	cli client.Client,
	routes []gatewayv1.HTTPRoute,
	mutate func(*gatewayv1.HTTPRoute),
) error {
	for i := range routes {
		route := &routes[i]
		updated := route.DeepCopy()
		mutate(updated)
		if maps.Equal(route.Annotations, updated.Annotations) {
			continue
		}
		if err := cli.Update(ctx, updated); err != nil {
			return fmt.Errorf("failed to update HTTPRoute %s/%s: %w", route.Namespace, route.Name, err)
		}
	}
	return nil
}
