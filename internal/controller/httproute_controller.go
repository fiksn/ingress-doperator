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
	"reflect"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/fiksn/ingress-doperator/internal/translator"
	"github.com/fiksn/ingress-doperator/internal/utils"
)

// HTTPRouteReconciler reconciles HTTPRoute resources and manages Gateway listeners
type HTTPRouteReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	GatewayNamespace    string
	GatewayName         string
	GatewayClassName    string
	HostnameRewriteFrom string
	HostnameRewriteTo   string
}

const (
	HTTPRouteFinalizerName = "ingress-doperator.fiction.si/httproute-finalizer"
)

// Reconcile manages Gateway listeners based on HTTPRoute changes
func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the HTTPRoute
	httpRoute := &gatewayv1.HTTPRoute{}
	err := r.Get(ctx, req.NamespacedName, httpRoute)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// HTTPRoute was deleted (finalizer already removed or none existed)
			logger.V(1).Info("HTTPRoute not found, already deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch HTTPRoute")
		return ctrl.Result{}, err
	}

	// Check if this HTTPRoute is managed by ingress-doperator
	if !r.isManagedByUs(httpRoute) {
		logger.V(1).Info("Skipping HTTPRoute not managed by ingress-doperator")
		return ctrl.Result{}, nil
	}

	// Handle deletion with finalizer
	if !httpRoute.DeletionTimestamp.IsZero() {
		// HTTPRoute is being deleted
		if utils.ContainsString(httpRoute.Finalizers, HTTPRouteFinalizerName) {
			// Our finalizer is present - do cleanup
			logger.Info("HTTPRoute being deleted, cleaning up Gateway listeners", "namespace", req.Namespace, "name", req.Name)
			if err := r.handleHTTPRouteDelete(ctx, httpRoute); err != nil {
				logger.Error(err, "failed to cleanup Gateway for deleted HTTPRoute")
				return ctrl.Result{}, err
			}

			// Remove our finalizer
			httpRoute.Finalizers = utils.RemoveString(httpRoute.Finalizers, HTTPRouteFinalizerName)
			if err := r.Update(ctx, httpRoute); err != nil {
				logger.Error(err, "failed to remove finalizer")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !utils.ContainsString(httpRoute.Finalizers, HTTPRouteFinalizerName) {
		httpRoute.Finalizers = append(httpRoute.Finalizers, HTTPRouteFinalizerName)
		if err := r.Update(ctx, httpRoute); err != nil {
			logger.Error(err, "failed to add finalizer")
			return ctrl.Result{}, err
		}
		logger.V(1).Info("Added finalizer to HTTPRoute")
	}

	// HTTPRoute created/updated - add/update listener on Gateway
	logger.V(1).Info("HTTPRoute created/updated, updating Gateway listener")
	return r.handleHTTPRouteCreateOrUpdate(ctx, httpRoute)
}

// isManagedByUs checks if the HTTPRoute is managed by ingress-doperator
func (r *HTTPRouteReconciler) isManagedByUs(httpRoute *gatewayv1.HTTPRoute) bool {
	if httpRoute.Annotations == nil {
		return false
	}
	managedBy, ok := httpRoute.Annotations[translator.ManagedByAnnotation]
	return ok && managedBy == translator.ManagedByValue
}

// handleHTTPRouteCreateOrUpdate adds or updates Gateway listener for the HTTPRoute
func (r *HTTPRouteReconciler) handleHTTPRouteCreateOrUpdate(ctx context.Context, httpRoute *gatewayv1.HTTPRoute) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Get the Gateway name from HTTPRoute's parent refs
	gatewayName := r.getGatewayNameFromHTTPRoute(httpRoute)
	if gatewayName == "" {
		logger.V(1).Info("HTTPRoute has no parent gateway reference, skipping")
		return ctrl.Result{}, nil
	}

	// Ensure the Gateway exists (create if not exists, fetch if exists)
	gatewayNN := types.NamespacedName{
		Namespace: r.GatewayNamespace,
		Name:      gatewayName,
	}
	gateway := &gatewayv1.Gateway{}
	err := r.Get(ctx, gatewayNN, gateway)

	gatewayExists := true
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Gateway doesn't exist - create it
			gatewayExists = false
			gateway = r.createInitialGateway(gatewayName)
			logger.Info("Creating new Gateway", "gateway", gatewayNN)
			if err := r.Create(ctx, gateway); err != nil {
				if apierrors.IsAlreadyExists(err) {
					// Race condition - another reconcile created it, retry
					logger.V(1).Info("Gateway already exists (race), retrying")
					return ctrl.Result{Requeue: true}, nil
				}
				logger.Error(err, "failed to create Gateway")
				return ctrl.Result{}, err
			}
			// Gateway created, continue to add listeners
		} else {
			logger.Error(err, "unable to fetch Gateway")
			return ctrl.Result{}, err
		}
	}

	// Check if we can manage this Gateway (only for existing gateways)
	if gatewayExists {
		canManage, err := utils.CanUpdateResource(ctx, r.Client, gateway, gatewayNN)
		if err != nil {
			logger.Error(err, "error checking Gateway resource")
			return ctrl.Result{}, err
		}
		if !canManage {
			logger.V(1).Info("Cannot manage Gateway (not owned by us)", "gateway", gatewayNN)
			return ctrl.Result{}, nil
		}
	}

	// Ensure ReferenceGrant exists if HTTPRoute is in a different namespace than Gateway
	// This must be done BEFORE updating the Gateway
	if httpRoute.Namespace != r.GatewayNamespace {
		if err := r.ensureReferenceGrant(ctx, httpRoute); err != nil {
			logger.Error(err, "failed to ensure ReferenceGrant")
			// Don't fail the reconcile, just log the error
		}
	}

	// Merge this HTTPRoute into existing Gateway listeners (no removals here)
	updated, err := r.updateGatewayListeners(ctx, gateway, httpRoute)
	if err != nil {
		logger.Error(err, "failed to update Gateway listeners")
		return ctrl.Result{}, err
	}

	if updated {
		if err := r.Update(ctx, gateway); err != nil {
			logger.Error(err, "failed to update Gateway")
			return ctrl.Result{}, err
		}
		logger.Info("Updated Gateway listeners", "gateway", gatewayNN)
	}

	return ctrl.Result{}, nil
}

// handleHTTPRouteDelete removes listeners/namespaces from Gateway when HTTPRoute is deleted
// With finalizer, we still have access to HTTPRoute spec for surgical cleanup
func (r *HTTPRouteReconciler) handleHTTPRouteDelete(ctx context.Context, httpRoute *gatewayv1.HTTPRoute) error {
	logger := log.FromContext(ctx)

	// Clean up ReferenceGrant for this HTTPRoute's namespace
	if httpRoute.Namespace != r.GatewayNamespace {
		if err := r.cleanupReferenceGrant(ctx, httpRoute.Namespace, httpRoute.Name); err != nil {
			logger.Error(err, "failed to cleanup ReferenceGrant", "namespace", httpRoute.Namespace, "name", httpRoute.Name)
			// Don't fail - continue with Gateway cleanup
		}
	}

	// Get the Gateway name from the HTTPRoute (we can still read spec!)
	gatewayName := r.getGatewayNameFromHTTPRoute(httpRoute)
	if gatewayName == "" {
		logger.V(1).Info("HTTPRoute has no parent gateway reference, nothing to clean up")
		return nil
	}

	// Fetch the Gateway
	gatewayNN := types.NamespacedName{
		Namespace: r.GatewayNamespace,
		Name:      gatewayName,
	}
	gateway := &gatewayv1.Gateway{}
	if err := r.Get(ctx, gatewayNN, gateway); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("Gateway not found, nothing to clean up", "gateway", gatewayNN)
			return nil
		}
		return err
	}

	// Check if we can manage this Gateway
	canManage, err := utils.CanUpdateResource(ctx, r.Client, gateway, gatewayNN)
	if err != nil {
		return err
	}
	if !canManage {
		logger.V(1).Info("Cannot manage Gateway, skipping cleanup", "gateway", gatewayNN)
		return nil
	}

	// Reconcile Gateway listeners based on remaining HTTPRoutes (exclude this one)
	routes, err := r.listHTTPRoutesForGateway(ctx, gatewayName, fmt.Sprintf("%s/%s", httpRoute.Namespace, httpRoute.Name))
	if err != nil {
		return err
	}
	updated, err := r.reconcileGatewayListeners(ctx, gatewayName, routes)
	if err != nil {
		return err
	}

	// Reload Gateway to check current listeners after reconciliation
	if err := r.Get(ctx, gatewayNN, gateway); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	// Check if Gateway has no listeners left - delete it if empty
	if len(gateway.Spec.Listeners) == 0 {
		// Gateway is empty, delete it
		logger.Info("Deleting Gateway (no listeners remain)", "gateway", gatewayNN)
		if err := r.Delete(ctx, gateway); err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
		}
		return nil
	}

	// Update Gateway if we made changes
	if updated {
		logger.Info("Updated Gateway after HTTPRoute deletion", "gateway", gatewayNN)
	}

	return nil
}

// reconcileGatewayListeners ensures a Gateway has exactly the listeners needed for the given routes
// This is an incremental operation: it only adds/removes what's necessary
func (r *HTTPRouteReconciler) reconcileGatewayListeners(
	ctx context.Context,
	gatewayName string,
	routes []gatewayv1.HTTPRoute,
) (bool, error) {
	logger := log.FromContext(ctx)

	gatewayNN := types.NamespacedName{
		Namespace: r.GatewayNamespace,
		Name:      gatewayName,
	}
	gateway := &gatewayv1.Gateway{}
	if err := r.Get(ctx, gatewayNN, gateway); err != nil {
		if apierrors.IsNotFound(err) {
			// Gateway doesn't exist, nothing to do
			return false, nil
		}
		return false, err
	}

	// Check if we can manage this Gateway
	canManage, err := utils.CanUpdateResource(ctx, r.Client, gateway, gatewayNN)
	if err != nil {
		return false, err
	}
	if !canManage {
		logger.V(1).Info("Cannot manage Gateway, skipping reconciliation", "gateway", gatewayNN)
		return false, nil
	}

	// Build desired state: which hostnames should have listeners with which namespaces
	desiredState := r.calculateDesiredListenerState(routes)

	// Update Gateway listeners to match desired state (incremental updates)
	desiredTLS, certMismatches := r.buildDesiredListenerTLS(ctx, desiredState, routes)

	updated := r.reconcileListenersToDesiredState(gateway, desiredState, desiredTLS, logger)

	desiredMismatch := ""
	if len(certMismatches) > 0 {
		desiredMismatch = strings.Join(certMismatches, "; ")
	}
	if gateway.Annotations == nil {
		gateway.Annotations = make(map[string]string)
	}
	if gateway.Annotations[translator.MismatchedCertAnnotation] != desiredMismatch {
		updated = true
		if desiredMismatch == "" {
			delete(gateway.Annotations, translator.MismatchedCertAnnotation)
		} else {
			gateway.Annotations[translator.MismatchedCertAnnotation] = desiredMismatch
		}
	}

	if updated {
		if err := r.Update(ctx, gateway); err != nil {
			return false, err
		}
		logger.Info("Reconciled Gateway listeners", "gateway", gatewayNN, "listenerCount", len(gateway.Spec.Listeners))
	}

	return updated, nil
}

// calculateDesiredListenerState computes which listeners should exist based on HTTPRoutes
func (r *HTTPRouteReconciler) calculateDesiredListenerState(routes []gatewayv1.HTTPRoute) map[string]map[string]bool {
	// hostname -> set of namespaces that need access
	state := make(map[string]map[string]bool)

	for _, route := range routes {
		for _, hostname := range route.Spec.Hostnames {
			hostnameStr := string(hostname)
			if _, exists := state[hostnameStr]; !exists {
				state[hostnameStr] = make(map[string]bool)
			}
			state[hostnameStr][route.Namespace] = true
		}
	}

	return state
}

// reconcileListenersToDesiredState incrementally updates Gateway listeners
// Returns true if Gateway was modified
func (r *HTTPRouteReconciler) reconcileListenersToDesiredState(
	gateway *gatewayv1.Gateway,
	desiredState map[string]map[string]bool,
	desiredTLS map[string]*gatewayv1.ListenerTLSConfig,
	logger logr.Logger,
) bool {
	updated := false

	// Step 1: Remove listeners that shouldn't exist
	newListeners := make([]gatewayv1.Listener, 0, len(gateway.Spec.Listeners))
	for _, listener := range gateway.Spec.Listeners {
		hostname := ""
		if listener.Hostname != nil {
			hostname = string(*listener.Hostname)
		}

		// Keep listener if it's in desired state
		if _, shouldExist := desiredState[hostname]; shouldExist {
			newListeners = append(newListeners, listener)
		} else {
			// Remove listener
			logger.Info("Removing listener (no routes reference it)", "listener", listener.Name, "hostname", hostname)
			updated = true
		}
	}
	gateway.Spec.Listeners = newListeners

	// Step 2: Update existing listeners and add new ones
	for hostname, namespaces := range desiredState {
		// Find existing listener
		listenerIdx := r.findListenerByHostname(gateway, hostname)

		// Convert namespace set to sorted slice
		namespaceList := make([]string, 0, len(namespaces))
		for ns := range namespaces {
			namespaceList = append(namespaceList, ns)
		}
		sort.Strings(namespaceList)

		if listenerIdx >= 0 {
			// Update existing listener's allowed namespaces
			if r.updateListenerNamespaces(&gateway.Spec.Listeners[listenerIdx], namespaceList) {
				logger.Info("Updated listener namespaces", "listener", gateway.Spec.Listeners[listenerIdx].Name, "namespaces", namespaceList)
				updated = true
			}
			if r.updateListenerTLS(&gateway.Spec.Listeners[listenerIdx], desiredTLS[hostname]) {
				logger.Info("Updated listener TLS", "listener", gateway.Spec.Listeners[listenerIdx].Name)
				updated = true
			}
		} else {
			// Add new listener
			listener := r.createListenerWithNamespaces(hostname, namespaceList, desiredTLS[hostname])
			gateway.Spec.Listeners = append(gateway.Spec.Listeners, listener)
			logger.Info("Added new listener", "listener", listener.Name, "hostname", hostname, "namespaces", namespaceList)
			updated = true
		}
	}

	return updated
}

// updateGatewayListeners merges this HTTPRoute into existing Gateway listeners (no removals)
func (r *HTTPRouteReconciler) updateGatewayListeners(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	httpRoute *gatewayv1.HTTPRoute,
) (bool, error) {
	logger := log.FromContext(ctx)
	updated := false

	desiredTLS, certMismatches := r.buildTLSForRoute(ctx, httpRoute)

	for _, hostname := range httpRoute.Spec.Hostnames {
		hostnameStr := string(hostname)
		listenerIdx := r.findListenerByHostname(gateway, hostnameStr)

		if listenerIdx >= 0 {
			if r.addNamespaceToListener(&gateway.Spec.Listeners[listenerIdx], httpRoute.Namespace) {
				updated = true
			}
			// Only set TLS if listener doesn't have it yet
			if gateway.Spec.Listeners[listenerIdx].TLS == nil && desiredTLS[hostnameStr] != nil {
				gateway.Spec.Listeners[listenerIdx].TLS = desiredTLS[hostnameStr]
				updated = true
			}
		} else {
			listener := r.createListenerWithNamespaces(hostnameStr, []string{httpRoute.Namespace}, desiredTLS[hostnameStr])
			gateway.Spec.Listeners = append(gateway.Spec.Listeners, listener)
			updated = true
			logger.Info("Added new listener", "listener", listener.Name, "hostname", hostname)
		}
	}

	if len(certMismatches) > 0 {
		desiredMismatch := strings.Join(certMismatches, "; ")
		if gateway.Annotations == nil {
			gateway.Annotations = make(map[string]string)
		}
		current := gateway.Annotations[translator.MismatchedCertAnnotation]
		merged := translator.MergeCertificateMismatchAnnotation(current, desiredMismatch)
		if merged != current {
			gateway.Annotations[translator.MismatchedCertAnnotation] = merged
			updated = true
		}
	}

	return updated, nil
}

// findListenerByHostname finds a listener index by hostname, returns -1 if not found
func (r *HTTPRouteReconciler) findListenerByHostname(gateway *gatewayv1.Gateway, hostname string) int {
	for i, listener := range gateway.Spec.Listeners {
		if listener.Hostname != nil && string(*listener.Hostname) == hostname {
			return i
		}
	}
	return -1
}

// updateListenerNamespaces updates a listener's allowed namespaces, returns true if changed
func (r *HTTPRouteReconciler) updateListenerNamespaces(listener *gatewayv1.Listener, namespaces []string) bool {
	if listener.AllowedRoutes == nil ||
		listener.AllowedRoutes.Namespaces == nil ||
		listener.AllowedRoutes.Namespaces.Selector == nil {
		return false
	}

	selector := listener.AllowedRoutes.Namespaces.Selector

	if selector.MatchLabels != nil {
		current, ok := selector.MatchLabels["kubernetes.io/metadata.name"]
		if len(namespaces) == 1 {
			if !ok || current != namespaces[0] {
				if selector.MatchLabels == nil {
					selector.MatchLabels = make(map[string]string)
				}
				selector.MatchLabels["kubernetes.io/metadata.name"] = namespaces[0]
				return true
			}
			return false
		}

		// Convert MatchLabels to MatchExpressions for multiple namespaces
		selector.MatchLabels = nil
		selector.MatchExpressions = []metav1.LabelSelectorRequirement{
			{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   namespaces,
			},
		}
		return true
	}

	for i := range listener.AllowedRoutes.Namespaces.Selector.MatchExpressions {
		expr := &listener.AllowedRoutes.Namespaces.Selector.MatchExpressions[i]
		if expr.Key == "kubernetes.io/metadata.name" && expr.Operator == metav1.LabelSelectorOpIn {
			// Check if namespaces changed
			if !stringSlicesEqual(expr.Values, namespaces) {
				expr.Values = namespaces
				return true
			}
			return false
		}
	}

	return false
}

func (r *HTTPRouteReconciler) updateListenerTLS(listener *gatewayv1.Listener, desiredTLS *gatewayv1.ListenerTLSConfig) bool {
	if reflect.DeepEqual(listener.TLS, desiredTLS) {
		return false
	}
	listener.TLS = desiredTLS
	return true
}

// stringSlicesEqual checks if two string slices are equal (assuming both are sorted)
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// createListenerWithNamespaces creates a listener with multiple allowed namespaces
func (r *HTTPRouteReconciler) createListenerWithNamespaces(
	hostname string,
	namespaces []string,
	tlsConfig *gatewayv1.ListenerTLSConfig,
) gatewayv1.Listener {
	from := gatewayv1.NamespacesFromSelector
	return gatewayv1.Listener{
		Name:     gatewayv1.SectionName(hostname),
		Hostname: (*gatewayv1.Hostname)(&hostname),
		Port:     gatewayv1.PortNumber(443),
		Protocol: gatewayv1.HTTPSProtocolType,
		TLS:      tlsConfig,
		AllowedRoutes: &gatewayv1.AllowedRoutes{
			Namespaces: &gatewayv1.RouteNamespaces{
				From: &from,
				Selector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "kubernetes.io/metadata.name",
							Operator: metav1.LabelSelectorOpIn,
							Values:   namespaces,
						},
					},
				},
			},
		},
	}
}

// addNamespaceToListener adds a namespace to the listener's allowed routes if not present
func (r *HTTPRouteReconciler) addNamespaceToListener(listener *gatewayv1.Listener, namespace string) bool {
	if listener.AllowedRoutes == nil ||
		listener.AllowedRoutes.Namespaces == nil ||
		listener.AllowedRoutes.Namespaces.Selector == nil {
		return false
	}

	for i := range listener.AllowedRoutes.Namespaces.Selector.MatchExpressions {
		expr := &listener.AllowedRoutes.Namespaces.Selector.MatchExpressions[i]
		if expr.Key == "kubernetes.io/metadata.name" && expr.Operator == metav1.LabelSelectorOpIn {
			// Check if namespace already exists
			for _, ns := range expr.Values {
				if ns == namespace {
					return false // Already present
				}
			}
			// Add namespace
			expr.Values = append(expr.Values, namespace)
			sort.Strings(expr.Values) // Keep sorted
			return true
		}
	}

	return false
}

// removeNamespaceFromListener removes a namespace from the listener's allowed routes
func (r *HTTPRouteReconciler) removeNamespaceFromListener(listener *gatewayv1.Listener, namespace string) bool {
	if listener.AllowedRoutes == nil ||
		listener.AllowedRoutes.Namespaces == nil ||
		listener.AllowedRoutes.Namespaces.Selector == nil {
		return false
	}

	for i := range listener.AllowedRoutes.Namespaces.Selector.MatchExpressions {
		expr := &listener.AllowedRoutes.Namespaces.Selector.MatchExpressions[i]
		if expr.Key == "kubernetes.io/metadata.name" && expr.Operator == metav1.LabelSelectorOpIn {
			// Find and remove namespace
			newValues := make([]string, 0, len(expr.Values))
			found := false
			for _, ns := range expr.Values {
				if ns != namespace {
					newValues = append(newValues, ns)
				} else {
					found = true
				}
			}
			if found {
				expr.Values = newValues
				return true
			}
			return false
		}
	}

	return false
}

// hasNoAllowedNamespaces checks if a listener has no allowed namespaces
func (r *HTTPRouteReconciler) hasNoAllowedNamespaces(listener *gatewayv1.Listener) bool {
	if listener.AllowedRoutes == nil ||
		listener.AllowedRoutes.Namespaces == nil ||
		listener.AllowedRoutes.Namespaces.Selector == nil {
		return true
	}

	for _, expr := range listener.AllowedRoutes.Namespaces.Selector.MatchExpressions {
		if expr.Key == "kubernetes.io/metadata.name" && expr.Operator == metav1.LabelSelectorOpIn {
			return len(expr.Values) == 0
		}
	}

	return true
}

// getGatewayNameFromHTTPRoute extracts the Gateway name from HTTPRoute parent refs
func (r *HTTPRouteReconciler) getGatewayNameFromHTTPRoute(httpRoute *gatewayv1.HTTPRoute) string {
	for _, parentRef := range httpRoute.Spec.ParentRefs {
		// Check if namespace matches (or is unset, defaulting to route namespace)
		ns := httpRoute.Namespace
		if parentRef.Namespace != nil {
			ns = string(*parentRef.Namespace)
		}
		if ns == r.GatewayNamespace {
			return string(parentRef.Name)
		}
	}
	return ""
}

// ensureReferenceGrant creates or updates a ReferenceGrant for the HTTPRoute's namespace
// Tracks the HTTPRoute in the source annotation (no ownerReference to avoid premature deletion)
func (r *HTTPRouteReconciler) ensureReferenceGrant(ctx context.Context, httpRoute *gatewayv1.HTTPRoute) error {
	logger := log.FromContext(ctx)

	refGrantName := translator.ReferenceGrantName
	refGrant := &gatewayv1beta1.ReferenceGrant{}
	refGrantNN := types.NamespacedName{
		Namespace: httpRoute.Namespace,
		Name:      refGrantName,
	}

	httpRouteKey := fmt.Sprintf("%s/%s", httpRoute.Namespace, httpRoute.Name)

	err := r.Get(ctx, refGrantNN, refGrant)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}

		// Create new ReferenceGrant
		newRefGrant := &gatewayv1beta1.ReferenceGrant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      refGrantName,
				Namespace: httpRoute.Namespace,
				Annotations: map[string]string{
					translator.ManagedByAnnotation: translator.ManagedByValue,
					translator.SourceAnnotation:    httpRouteKey,
				},
			},
			Spec: gatewayv1beta1.ReferenceGrantSpec{
				From: []gatewayv1beta1.ReferenceGrantFrom{
					{
						Group:     gatewayv1.GroupName,
						Kind:      "Gateway",
						Namespace: gatewayv1.Namespace(r.GatewayNamespace),
					},
				},
				To: []gatewayv1beta1.ReferenceGrantTo{
					{
						Group: "",
						Kind:  "Secret",
					},
				},
			},
		}

		logger.Info("Creating ReferenceGrant", "namespace", httpRoute.Namespace, "name", refGrantName, "source", httpRouteKey)
		if err := r.Create(ctx, newRefGrant); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Race condition - retry to update it
				return r.ensureReferenceGrant(ctx, httpRoute)
			}
			return err
		}
		return nil
	}

	// Skip ReferenceGrant not managed by us
	if refGrant.Annotations == nil || refGrant.Annotations[translator.ManagedByAnnotation] != translator.ManagedByValue {
		logger.V(1).Info("ReferenceGrant not managed by us, skipping update", "namespace", httpRoute.Namespace, "name", refGrantName)
		return nil
	}

	if refGrant.Annotations == nil {
		refGrant.Annotations = make(map[string]string)
	}

	// ReferenceGrant exists - add this HTTPRoute to sources if not present
	sources := getSourcesFromAnnotation(refGrant.Annotations[translator.SourceAnnotation])
	if !utils.ContainsString(sources, httpRouteKey) {
		sources = append(sources, httpRouteKey)
		sort.Strings(sources)
		refGrant.Annotations[translator.SourceAnnotation] = strings.Join(sources, ",")

		logger.Info("Updating ReferenceGrant sources", "namespace", httpRoute.Namespace, "name", refGrantName, "addedSource", httpRouteKey)
		if err := r.Update(ctx, refGrant); err != nil {
			return err
		}
	}

	return nil
}

// cleanupReferenceGrant removes the HTTPRoute from ReferenceGrant sources and deletes if empty
func (r *HTTPRouteReconciler) cleanupReferenceGrant(ctx context.Context, namespace, name string) error {
	logger := log.FromContext(ctx)

	refGrantName := translator.ReferenceGrantName
	refGrant := &gatewayv1beta1.ReferenceGrant{}
	refGrantNN := types.NamespacedName{
		Namespace: namespace,
		Name:      refGrantName,
	}

	err := r.Get(ctx, refGrantNN, refGrant)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Already deleted, nothing to do
			return nil
		}
		return err
	}

	// Check if managed by us
	if refGrant.Annotations == nil || refGrant.Annotations[translator.ManagedByAnnotation] != translator.ManagedByValue {
		logger.V(1).Info("ReferenceGrant not managed by us, skipping cleanup", "namespace", namespace)
		return nil
	}

	httpRouteKey := fmt.Sprintf("%s/%s", namespace, name)

	// Remove this HTTPRoute from sources
	sources := getSourcesFromAnnotation(refGrant.Annotations[translator.SourceAnnotation])
	newSources := utils.RemoveString(sources, httpRouteKey)

	if len(newSources) == 0 {
		// No more HTTPRoutes use this ReferenceGrant - delete it
		logger.Info("Deleting ReferenceGrant (no HTTPRoutes remain)", "namespace", namespace, "name", refGrantName)
		if err := r.Delete(ctx, refGrant); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}

	if len(newSources) != len(sources) {
		// Update sources annotation
		refGrant.Annotations[translator.SourceAnnotation] = strings.Join(newSources, ",")
		logger.Info("Updating ReferenceGrant sources", "namespace", namespace, "name", refGrantName, "removedSource", httpRouteKey)
		if err := r.Update(ctx, refGrant); err != nil {
			return err
		}
	}

	return nil
}

// getSourcesFromAnnotation parses the comma-separated source annotation
func getSourcesFromAnnotation(annotation string) []string {
	if annotation == "" {
		return []string{}
	}
	parts := strings.Split(annotation, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func (r *HTTPRouteReconciler) listHTTPRoutesForGateway(
	ctx context.Context,
	gatewayName string,
	excludeKey string,
) ([]gatewayv1.HTTPRoute, error) {
	allRoutes := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, allRoutes); err != nil {
		return nil, err
	}

	routes := make([]gatewayv1.HTTPRoute, 0, len(allRoutes.Items))
	for _, route := range allRoutes.Items {
		if !route.DeletionTimestamp.IsZero() {
			continue
		}
		if excludeKey != "" && fmt.Sprintf("%s/%s", route.Namespace, route.Name) == excludeKey {
			continue
		}
		if r.getGatewayNameFromHTTPRoute(&route) != gatewayName {
			continue
		}
		routes = append(routes, route)
	}

	return routes, nil
}

type tlsCandidate struct {
	ingressKey       string
	ingressNamespace string
	originalHost     string
	transformedHost  string
	tlsConfig        *networkingv1.IngressTLS
}

func (r *HTTPRouteReconciler) buildDesiredListenerTLS(
	ctx context.Context,
	desiredState map[string]map[string]bool,
	routes []gatewayv1.HTTPRoute,
) (map[string]*gatewayv1.ListenerTLSConfig, []string) {
	desiredTLS := make(map[string]*gatewayv1.ListenerTLSConfig, len(desiredState))
	certMismatches := make([]string, 0)

	trans := translator.New(translator.Config{
		GatewayNamespace:    r.GatewayNamespace,
		HostnameRewriteFrom: r.HostnameRewriteFrom,
		HostnameRewriteTo:   r.HostnameRewriteTo,
	})

	bestCandidates := make(map[string]tlsCandidate)

	for i := range routes {
		route := &routes[i]
		if !r.isManagedByUs(route) {
			continue
		}

		source := ""
		if route.Annotations != nil {
			source = route.Annotations[translator.SourceAnnotation]
		}
		if source == "" {
			continue
		}

		parts := strings.SplitN(source, "/", 2)
		if len(parts) != 2 {
			continue
		}
		ingressNamespace, ingressName := parts[0], parts[1]

		ingress := &networkingv1.Ingress{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: ingressNamespace, Name: ingressName}, ingress); err != nil {
			continue
		}

		routeHosts := make(map[string]bool)
		for _, host := range route.Spec.Hostnames {
			routeHosts[string(host)] = true
		}

		for _, rule := range ingress.Spec.Rules {
			if rule.Host == "" {
				continue
			}
			transformed := trans.TransformHostname(rule.Host)
			if !routeHosts[transformed] {
				continue
			}

			tlsConfig := findTLSConfigForHost(ingress, rule.Host)
			if tlsConfig == nil || tlsConfig.SecretName == "" {
				continue
			}

			candidate := tlsCandidate{
				ingressKey:       fmt.Sprintf("%s/%s", ingressNamespace, ingressName),
				ingressNamespace: ingressNamespace,
				originalHost:     rule.Host,
				transformedHost:  transformed,
				tlsConfig:        tlsConfig,
			}

			existing, exists := bestCandidates[transformed]
			if !exists || candidate.ingressKey < existing.ingressKey {
				bestCandidates[transformed] = candidate
			}
		}
	}

	for hostname := range desiredState {
		candidate, ok := bestCandidates[hostname]
		if !ok {
			desiredTLS[hostname] = nil
			continue
		}

		secretName := candidate.tlsConfig.SecretName
		secretNamespace := candidate.ingressNamespace

		if candidate.originalHost != candidate.transformedHost &&
			!trans.CheckCertificateMatch(candidate.originalHost, candidate.transformedHost, candidate.tlsConfig.Hosts) {
			newSecretName := generateSafeSecretName(candidate.ingressNamespace, candidate.transformedHost)
			certMismatches = append(certMismatches,
				fmt.Sprintf("%s->%s: %s/%s->%s/%s",
					candidate.originalHost, candidate.transformedHost,
					candidate.ingressNamespace, candidate.tlsConfig.SecretName,
					r.GatewayNamespace, newSecretName))
			secretName = newSecretName
			secretNamespace = r.GatewayNamespace
		}

		mode := gatewayv1.TLSModeTerminate
		desiredTLS[hostname] = &gatewayv1.ListenerTLSConfig{
			Mode: &mode,
			CertificateRefs: []gatewayv1.SecretObjectReference{
				{
					Name:      gatewayv1.ObjectName(secretName),
					Namespace: (*gatewayv1.Namespace)(&secretNamespace),
				},
			},
		}
	}

	sort.Strings(certMismatches)
	return desiredTLS, certMismatches
}

func findTLSConfigForHost(ingress *networkingv1.Ingress, host string) *networkingv1.IngressTLS {
	for _, tls := range ingress.Spec.TLS {
		for _, tlsHost := range tls.Hosts {
			if tlsHost == host {
				return &tls
			}
		}
	}
	return nil
}

// generateSafeSecretName mirrors translator.generateSafeSecretName.
func generateSafeSecretName(namespace, hostname string) string {
	safeName := fmt.Sprintf("automatic-%s-%s-tls",
		strings.ReplaceAll(namespace, ".", "-"),
		strings.ReplaceAll(hostname, ".", "-"))
	if len(safeName) <= translator.MaxK8sNameLength {
		return safeName
	}
	excess := len(safeName) - translator.MaxK8sNameLength
	if excess <= 0 {
		return safeName
	}
	if excess >= len(hostname) {
		return safeName[:translator.MaxK8sNameLength]
	}
	shortHost := hostname[:len(hostname)-excess]
	shortHost = strings.TrimRight(shortHost, "-")
	safeName = fmt.Sprintf("automatic-%s-%s-tls",
		strings.ReplaceAll(namespace, ".", "-"),
		strings.ReplaceAll(shortHost, ".", "-"))
	if len(safeName) > translator.MaxK8sNameLength {
		return safeName[:translator.MaxK8sNameLength]
	}
	return safeName
}

func (r *HTTPRouteReconciler) buildTLSForRoute(
	ctx context.Context,
	httpRoute *gatewayv1.HTTPRoute,
) (map[string]*gatewayv1.ListenerTLSConfig, []string) {
	desiredTLS := make(map[string]*gatewayv1.ListenerTLSConfig)
	certMismatches := make([]string, 0)

	if !r.isManagedByUs(httpRoute) {
		return desiredTLS, certMismatches
	}

	source := ""
	if httpRoute.Annotations != nil {
		source = httpRoute.Annotations[translator.SourceAnnotation]
	}
	if source == "" {
		return desiredTLS, certMismatches
	}

	parts := strings.SplitN(source, "/", 2)
	if len(parts) != 2 {
		return desiredTLS, certMismatches
	}
	ingressNamespace, ingressName := parts[0], parts[1]

	ingress := &networkingv1.Ingress{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ingressNamespace, Name: ingressName}, ingress); err != nil {
		return desiredTLS, certMismatches
	}

	trans := translator.New(translator.Config{
		GatewayNamespace:    r.GatewayNamespace,
		HostnameRewriteFrom: r.HostnameRewriteFrom,
		HostnameRewriteTo:   r.HostnameRewriteTo,
	})

	routeHosts := make(map[string]bool)
	for _, host := range httpRoute.Spec.Hostnames {
		routeHosts[string(host)] = true
	}

	for _, rule := range ingress.Spec.Rules {
		if rule.Host == "" {
			continue
		}

		transformed := trans.TransformHostname(rule.Host)
		if !routeHosts[transformed] {
			continue
		}

		tlsConfig := findTLSConfigForHost(ingress, rule.Host)
		if tlsConfig == nil || tlsConfig.SecretName == "" {
			continue
		}

		secretName := tlsConfig.SecretName
		secretNamespace := ingressNamespace

		if rule.Host != transformed && !trans.CheckCertificateMatch(rule.Host, transformed, tlsConfig.Hosts) {
			newSecretName := generateSafeSecretName(ingressNamespace, transformed)
			certMismatches = append(certMismatches,
				fmt.Sprintf("%s->%s: %s/%s->%s/%s",
					rule.Host, transformed,
					ingressNamespace, tlsConfig.SecretName,
					r.GatewayNamespace, newSecretName))
			secretName = newSecretName
			secretNamespace = r.GatewayNamespace
		}

		mode := gatewayv1.TLSModeTerminate
		desiredTLS[transformed] = &gatewayv1.ListenerTLSConfig{
			Mode: &mode,
			CertificateRefs: []gatewayv1.SecretObjectReference{
				{
					Name:      gatewayv1.ObjectName(secretName),
					Namespace: (*gatewayv1.Namespace)(&secretNamespace),
				},
			},
		}
	}

	sort.Strings(certMismatches)
	return desiredTLS, certMismatches
}

// createInitialGateway creates a minimal Gateway resource
func (r *HTTPRouteReconciler) createInitialGateway(gatewayName string) *gatewayv1.Gateway {
	gatewayClassName := r.GatewayClassName
	if gatewayClassName == "" {
		gatewayClassName = "nginx" // Default from CLI flag
	}

	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gatewayName,
			Namespace: r.GatewayNamespace,
			Annotations: map[string]string{
				translator.ManagedByAnnotation: translator.ManagedByValue,
			},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gatewayClassName),
			Listeners:        []gatewayv1.Listener{}, // Empty, will be populated by reconciler
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		WithEventFilter(ManagedByIngressDoperatorPredicate()).
		Complete(r)
}

// ManagedByIngressDoperatorPredicate filters events to only process HTTPRoutes managed by ingress-doperator
func ManagedByIngressDoperatorPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		annotations := obj.GetAnnotations()
		if annotations == nil {
			return false
		}
		managedBy, ok := annotations[translator.ManagedByAnnotation]
		return ok && managedBy == translator.ManagedByValue
	})
}
