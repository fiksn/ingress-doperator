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
	"fmt"
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	MaxHTTPRouteRules = 16 // Gateway API limit
)

// HTTPRouteManager handles HTTPRoute operations
type HTTPRouteManager struct {
	Client client.Client
}

// GetHTTPRoutesWithPrefix returns all HTTPRoutes with a given name prefix
// that are managed by us for the specified Ingress
func (m *HTTPRouteManager) GetHTTPRoutesWithPrefix(
	ctx context.Context,
	namespace string,
	prefix string,
) ([]gatewayv1.HTTPRoute, error) {
	routeList := &gatewayv1.HTTPRouteList{}

	if err := m.Client.List(ctx, routeList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	var result []gatewayv1.HTTPRoute
	for _, r := range routeList.Items {
		if strings.HasPrefix(r.Name, prefix) && IsManagedByUsForIngress(&r, namespace, prefix) {
			result = append(result, r)
		}
	}

	return result, nil
}

// SplitHTTPRouteIfNeeded splits an HTTPRoute into multiple routes if it exceeds the Gateway API limit
func (m *HTTPRouteManager) SplitHTTPRouteIfNeeded(httpRoute *gatewayv1.HTTPRoute) []*gatewayv1.HTTPRoute {
	// If the HTTPRoute has <= MaxHTTPRouteRules, no split needed
	if len(httpRoute.Spec.Rules) <= MaxHTTPRouteRules {
		return []*gatewayv1.HTTPRoute{httpRoute}
	}

	logger := log.Log.WithName("SplitHTTPRouteIfNeeded")
	logger.Info("HTTPRoute exceeds max rules, splitting",
		"name", httpRoute.Name,
		"namespace", httpRoute.Namespace,
		"totalRules", len(httpRoute.Spec.Rules),
		"maxRules", MaxHTTPRouteRules)

	// Split into multiple HTTPRoutes
	var result []*gatewayv1.HTTPRoute
	rules := httpRoute.Spec.Rules
	partNum := 1

	for i := 0; i < len(rules); i += MaxHTTPRouteRules {
		end := i + MaxHTTPRouteRules
		if end > len(rules) {
			end = len(rules)
		}

		// Create a copy of the HTTPRoute for this chunk
		part := httpRoute.DeepCopy()
		part.Spec.Rules = rules[i:end]

		if partNum > 1 {
			part.Name = fmt.Sprintf("%s-%d", httpRoute.Name, partNum)
		}

		logger.Info("Created HTTPRoute part",
			"originalName", httpRoute.Name,
			"partName", part.Name,
			"partNum", partNum,
			"rulesInPart", len(part.Spec.Rules))

		result = append(result, part)
		partNum++
	}

	return result
}

// ApplyHTTPRoutes makes the Ingress's managed HTTPRoutes match desiredRoutes: managed routes that
// are no longer desired (e.g. split parts or an HTTP route that is not needed anymore) are deleted,
// then every desired route is created or updated in place, so unchanged routes are never recreated.
func (m *HTTPRouteManager) ApplyHTTPRoutes(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	desiredRoutes []*gatewayv1.HTTPRoute,
	metricRecorder func(operation, namespace, name string),
) error {
	logger := log.FromContext(ctx)

	existingRoutes, err := m.GetHTTPRoutesWithPrefix(ctx, ingress.Namespace, ingress.Name)
	if err != nil {
		return fmt.Errorf("failed to get existing HTTPRoutes: %w", err)
	}

	desiredNames := make(map[string]bool, len(desiredRoutes))
	for _, route := range desiredRoutes {
		desiredNames[route.Name] = true
	}
	for i := range existingRoutes {
		existingRoute := &existingRoutes[i]
		if desiredNames[existingRoute.Name] {
			continue
		}
		logger.Info("Deleting obsolete HTTPRoute", "namespace", existingRoute.Namespace, "name", existingRoute.Name)
		if err := m.Client.Delete(ctx, existingRoute); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete obsolete HTTPRoute %s/%s: %w",
				existingRoute.Namespace, existingRoute.Name, err)
		}
		if metricRecorder != nil {
			metricRecorder("delete", existingRoute.Namespace, existingRoute.Name)
		}
	}

	for _, desiredRoute := range desiredRoutes {
		if err := m.applyHTTPRoute(ctx, desiredRoute, metricRecorder); err != nil {
			return fmt.Errorf("failed to apply HTTPRoute %s: %w", desiredRoute.Name, err)
		}
	}
	return nil
}

// applyHTTPRoute creates or updates a single HTTPRoute
func (m *HTTPRouteManager) applyHTTPRoute(
	ctx context.Context,
	httpRoute *gatewayv1.HTTPRoute,
	metricRecorder func(operation, namespace, name string),
) error {
	logger := log.FromContext(ctx)

	httpRouteNN := types.NamespacedName{
		Namespace: httpRoute.Namespace,
		Name:      httpRoute.Name,
	}
	existingHTTPRoute := &gatewayv1.HTTPRoute{}
	canManage, err := CanUpdateResource(ctx, m.Client, existingHTTPRoute, httpRouteNN)
	if err != nil {
		return err
	}

	if !canManage {
		logger.Info("Skipping HTTPRoute synthesis - resource exists and is not managed by us",
			"namespace", httpRouteNN.Namespace, "name", httpRouteNN.Name)
		return nil
	}

	// Check if HTTPRoute exists
	err = m.Client.Get(ctx, httpRouteNN, existingHTTPRoute)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// Create new HTTPRoute
			logger.Info("Creating HTTPRoute", "namespace", httpRoute.Namespace, "name", httpRoute.Name)
			if err := m.Client.Create(ctx, httpRoute); err != nil {
				return fmt.Errorf("failed to create HTTPRoute: %w", err)
			}
			if metricRecorder != nil {
				metricRecorder("create", httpRoute.Namespace, httpRoute.Name)
			}
			return nil
		}
		return err
	}

	// Update existing HTTPRoute, keeping the listener bookkeeping owned by the HTTPRoute controller
	listenerGateway, hasListenerGateway := existingHTTPRoute.Annotations[ListenerGatewayAnnotation]
	existingHTTPRoute.Annotations = maps.Clone(httpRoute.Annotations)
	if hasListenerGateway {
		if existingHTTPRoute.Annotations == nil {
			existingHTTPRoute.Annotations = make(map[string]string)
		}
		existingHTTPRoute.Annotations[ListenerGatewayAnnotation] = listenerGateway
	}
	existingHTTPRoute.Spec = httpRoute.Spec
	logger.Info("Updating HTTPRoute", "namespace", existingHTTPRoute.Namespace, "name", existingHTTPRoute.Name)
	if err := m.Client.Update(ctx, existingHTTPRoute); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("HTTPRoute disappeared during update, recreating",
				"namespace", existingHTTPRoute.Namespace,
				"name", existingHTTPRoute.Name)
			if err := m.Client.Create(ctx, httpRoute); err != nil {
				return fmt.Errorf("failed to recreate HTTPRoute: %w", err)
			}
			if metricRecorder != nil {
				metricRecorder("create", httpRoute.Namespace, httpRoute.Name)
			}
			return nil
		}
		return fmt.Errorf("failed to update HTTPRoute: %w", err)
	}
	if metricRecorder != nil {
		metricRecorder("update", existingHTTPRoute.Namespace, existingHTTPRoute.Name)
	}

	return nil
}

// WithResolvedNamedPorts returns a copy of ingress whose named Service ports are replaced by the
// port numbers of the Services, so every path keeps its own port: an HTTPRoute backend only
// carries the Service name, so paths using the same Service on different named ports could not be
// told apart after translation. Ports whose Service or port cannot be found stay named and are
// returned as "service:port" in unresolved; callers must not translate the Ingress then.
func (m *HTTPRouteManager) WithResolvedNamedPorts(
	ctx context.Context,
	ingress *networkingv1.Ingress,
) (resolved *networkingv1.Ingress, unresolved []string) {
	logger := log.FromContext(ctx)
	resolved = ingress.DeepCopy()
	for i := range resolved.Spec.Rules {
		http := resolved.Spec.Rules[i].HTTP
		if http == nil {
			continue
		}
		for j := range http.Paths {
			service := http.Paths[j].Backend.Service
			if service == nil || service.Port.Name == "" {
				continue
			}
			port, err := m.resolveServicePort(ctx, resolved.Namespace, service.Name, service.Port.Name)
			if err != nil {
				logger.Info("Could not resolve named port from Service", "namespace", resolved.Namespace,
					"service", service.Name, "portName", service.Port.Name, "error", err.Error())
				unresolved = append(unresolved, service.Name+":"+service.Port.Name)
				continue
			}
			logger.Info("Resolved named port from Service", "namespace", resolved.Namespace,
				"service", service.Name, "portName", service.Port.Name, "port", port)
			service.Port = networkingv1.ServiceBackendPort{Number: port}
		}
	}
	return resolved, unresolved
}

// ReferencesServiceByPortName reports whether a path of ingress uses the Service by a named port,
// which makes the Ingress's HTTPRoutes depend on the Service's port numbers.
func ReferencesServiceByPortName(ingress *networkingv1.Ingress, serviceName string) bool {
	for _, rule := range ingress.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			service := path.Backend.Service
			if service != nil && service.Name == serviceName && service.Port.Name != "" {
				return true
			}
		}
	}
	return false
}

// resolveServicePort looks up a Service and resolves a named port to its numeric value
func (m *HTTPRouteManager) resolveServicePort(ctx context.Context, namespace, serviceName, portName string) (int32, error) {
	var service corev1.Service
	err := m.Client.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      serviceName,
	}, &service)
	if err != nil {
		return 0, fmt.Errorf("failed to get Service: %w", err)
	}

	// Find the port by name
	for _, port := range service.Spec.Ports {
		if port.Name == portName {
			return port.Port, nil
		}
	}

	return 0, fmt.Errorf("port %s not found in Service %s", portName, serviceName)
}
