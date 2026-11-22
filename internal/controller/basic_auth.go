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
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/utils"
)

// basicAuthRetryInterval is how often an Ingress skipped because its basic authentication could
// not be translated is checked again, e.g. after NGINX Gateway Fabric is upgraded. Changes to the
// Ingress itself are reconciled right away.
const basicAuthRetryInterval = 5 * time.Minute

// prepareBasicAuth generates the AuthenticationFilter for an Ingress using the ingress-nginx basic
// authentication annotations and returns its name, to be referenced by every rule of the Ingress's
// HTTPRoutes. When the authentication cannot be translated, skipped is true: the Ingress gets a
// warning and its HTTPRoutes are deleted, so its hosts are never served without authentication.
// An AuthenticationFilter named with httproute-authentication-filter takes precedence.
func (r *IngressReconciler) prepareBasicAuth(
	ctx context.Context,
	ingress *networkingv1.Ingress,
) (filterName string, skipped bool, err error) {
	if !utils.RequestsAuthentication(ingress.Annotations) {
		return "", false, nil
	}
	if len(utils.ParseCommaSeparatedAnnotation(ingress.Annotations, HTTPRouteAuthenticationAnnotation)) > 0 {
		r.recordWarning(ingress, "BasicAuthIgnored", fmt.Sprintf("%s is not translated: the AuthenticationFilter "+
			"named in %s is used instead", utils.AuthTypeAnnotation, HTTPRouteAuthenticationAnnotation))
		return "", false, nil
	}
	auth, err := utils.BasicAuthFor(ingress.Annotations, ingress.Namespace)
	if err != nil {
		return "", true, r.skipUntranslatableAuth(ctx, ingress, err.Error())
	}
	var owner client.Object = ingress
	if r.IngressPostProcessingMode == IngressPostProcessingModeRemove {
		owner = nil
	}
	ready, err := utils.EnsureBasicAuthFilter(ctx, r.Client, r.Scheme, owner, ingress.Namespace, ingress.Name, *auth)
	if err != nil {
		return "", true, fmt.Errorf("failed to apply AuthenticationFilter: %w", err)
	}
	filterName = utils.BasicAuthFilterName(ingress.Name)
	if !ready {
		return "", true, r.skipUntranslatableAuth(ctx, ingress, fmt.Sprintf("the AuthenticationFilter CRD "+
			"(NGINX Gateway Fabric 2.4+) is not installed, or AuthenticationFilter %s/%s exists and is not "+
			"managed by ingress-doperator", ingress.Namespace, filterName))
	}
	return filterName, false, nil
}

// skipUntranslatableAuth warns that the Ingress is not translated because of its authentication
// and deletes the HTTPRoutes generated for it earlier.
func (r *IngressReconciler) skipUntranslatableAuth(ctx context.Context, ingress *networkingv1.Ingress, reason string) error {
	log.FromContext(ctx).Info("Skipping Ingress with untranslatable authentication",
		"namespace", ingress.Namespace, "name", ingress.Name, "reason", reason)
	r.recordWarning(ingress, "BasicAuthUnsupported", fmt.Sprintf("not translated, so the hosts are not "+
		"served without authentication: %s", reason))
	if err := r.HTTPRouteManager.ApplyHTTPRoutes(ctx, ingress, nil, nil); err != nil {
		return fmt.Errorf("failed to delete HTTPRoutes of skipped Ingress: %w", err)
	}
	return nil
}

// requireBasicAuth adds the AuthenticationFilter to every rule of the routes.
func requireBasicAuth(routes []*gatewayv1.HTTPRoute, filterName string) {
	if filterName == "" {
		return
	}
	for _, route := range routes {
		utils.AddExtensionRefFilters(route, utils.NginxGatewayGroup, utils.AuthenticationFilterKind, []string{filterName})
	}
}
