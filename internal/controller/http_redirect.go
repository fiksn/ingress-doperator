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
	"maps"
	"net/http"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/translator"
)

const (
	// httpListenerName is the catch-all port 80 listener added with --http-redirect.
	httpListenerName = "http"
	httpListenerPort = 80
	// httpRouteSuffix names the HTTPRoute carrying the hostnames an Ingress serves over plain HTTP.
	httpRouteSuffix = "-http"
	// httpsRedirectRouteSuffix names the per-Gateway HTTPRoute redirecting plain HTTP to HTTPS.
	httpsRedirectRouteSuffix = "-https-redirect"
	// HTTPSRedirectRouteAnnotation marks the redirect HTTPRoutes ingress-doperator manages; the value
	// is the version of their spec.
	HTTPSRedirectRouteAnnotation = "ingress-doperator.fiction.si/https-redirect"
	httpsRedirectRouteVersion    = "v1"

	nginxSSLRedirectAnnotation      = "nginx.ingress.kubernetes.io/ssl-redirect"
	nginxForceSSLRedirectAnnotation = "nginx.ingress.kubernetes.io/force-ssl-redirect"
)

// isHTTPListener reports whether listener is the catch-all port 80 listener ingress-doperator
// manages with --http-redirect.
func isHTTPListener(listener gatewayv1.Listener) bool {
	return string(listener.Name) == httpListenerName &&
		listener.Protocol == gatewayv1.HTTPProtocolType &&
		listener.Port == httpListenerPort &&
		(listener.Hostname == nil || *listener.Hostname == "")
}

// attachesToHTTPListener reports whether httpRoute is attached to the catch-all port 80 listener,
// i.e. is an Ingress's plain HTTP route.
func attachesToHTTPListener(httpRoute *gatewayv1.HTTPRoute) bool {
	for _, ref := range httpRoute.Spec.ParentRefs {
		if ref.SectionName != nil && string(*ref.SectionName) == httpListenerName {
			return true
		}
	}
	return false
}

func isHTTPSRedirectRoute(httpRoute *gatewayv1.HTTPRoute) bool {
	_, ok := httpRoute.Annotations[HTTPSRedirectRouteAnnotation]
	return ok
}

// classifyHostnames splits the Ingress's (transformed) hostnames like ingress-nginx serves them:
// hostnames with a certificate (an Ingress TLS entry or a wildcard listener) are served over HTTPS
// and, with ssl-redirect "false", also over plain HTTP; hostnames without a certificate are served
// over plain HTTP only, unless force-ssl-redirect is "true".
func classifyHostnames(
	ingress *networkingv1.Ingress,
	transform func(string) string,
	wildcards []WildcardListener,
) (httpsHosts, httpHosts []gatewayv1.Hostname) {
	sslRedirectOff := strings.EqualFold(ingress.Annotations[nginxSSLRedirectAnnotation], "false")
	forceSSLRedirect := strings.EqualFold(ingress.Annotations[nginxForceSSLRedirectAnnotation], "true")
	seen := make(map[string]bool)
	for _, rule := range ingress.Spec.Rules {
		host := transform(rule.Host)
		if rule.Host == "" || seen[host] {
			continue
		}
		seen[host] = true
		hasCertificate := findTLSConfigForHost(ingress, rule.Host) != nil || listenerHostname(wildcards, host) != host
		if hasCertificate || forceSSLRedirect {
			httpsHosts = append(httpsHosts, gatewayv1.Hostname(host))
		}
		if (!hasCertificate && !forceSSLRedirect) || (hasCertificate && sslRedirectOff) {
			httpHosts = append(httpHosts, gatewayv1.Hostname(host))
		}
	}
	return httpsHosts, httpHosts
}

// httpListenerRoutes returns the HTTPRoutes serving the Ingress. Without --http-redirect that is
// httpRoute as is. With it, httpRoute keeps the hostnames served over HTTPS, and a <name>-http copy
// attached to the catch-all port 80 listener carries the hostnames served over plain HTTP; a route
// left without hostnames is dropped.
func (r *IngressReconciler) httpListenerRoutes(
	ingress *networkingv1.Ingress,
	trans *translator.Translator,
	httpRoute *gatewayv1.HTTPRoute,
) []*gatewayv1.HTTPRoute {
	if !r.HTTPRedirect || len(httpRoute.Spec.ParentRefs) == 0 {
		return []*gatewayv1.HTTPRoute{httpRoute}
	}
	httpsHosts, httpHosts := classifyHostnames(ingress, trans.TransformHostname, r.WildcardListeners)

	var routes []*gatewayv1.HTTPRoute
	if len(httpHosts) > 0 {
		plain := httpRoute.DeepCopy()
		plain.Name += httpRouteSuffix
		plain.Spec.Hostnames = httpHosts
		parent := plain.Spec.ParentRefs[0]
		parent.SectionName = ptr.To(gatewayv1.SectionName(httpListenerName))
		plain.Spec.ParentRefs = []gatewayv1.ParentReference{parent}
		routes = append(routes, plain)
	}
	if len(httpsHosts) > 0 {
		restrictToHostnames(httpRoute, httpsHosts, r.WildcardListeners)
		routes = append([]*gatewayv1.HTTPRoute{httpRoute}, routes...)
	}
	return routes
}

// restrictToHostnames keeps only hostnames on httpRoute, and only the parentRefs of their listeners.
func restrictToHostnames(httpRoute *gatewayv1.HTTPRoute, hostnames []gatewayv1.Hostname, wildcards []WildcardListener) {
	sections := make(map[string]bool, len(hostnames))
	for _, host := range listenerHostnames(wildcards, hostnames) {
		sections[translator.ListenerNameForHostname(string(host))] = true
	}
	refs := make([]gatewayv1.ParentReference, 0, len(httpRoute.Spec.ParentRefs))
	for _, ref := range httpRoute.Spec.ParentRefs {
		if ref.SectionName == nil || sections[string(*ref.SectionName)] {
			refs = append(refs, ref)
		}
	}
	httpRoute.Spec.Hostnames = hostnames
	httpRoute.Spec.ParentRefs = refs
}

// httpListenerNamespaces returns the namespaces allowed on the catch-all port 80 listener: the
// Gateway namespace (for the redirect route) and those of the plain HTTP routes, sorted.
func (r *HTTPRouteReconciler) httpListenerNamespaces(routes []gatewayv1.HTTPRoute) []string {
	set := map[string]bool{r.GatewayNamespace: true}
	for i := range routes {
		if attachesToHTTPListener(&routes[i]) {
			set[routes[i].Namespace] = true
		}
	}
	namespaces := make([]string, 0, len(set))
	for namespace := range set {
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)
	return namespaces
}

// ensureHTTPListener makes sure gateway has the catch-all port 80 listener allowing namespaces
// (--http-redirect). A listener named "http" of another shape belongs to someone else and is left
// alone. Returns true if the Gateway changed.
func (r *HTTPRouteReconciler) ensureHTTPListener(
	gateway *gatewayv1.Gateway,
	namespaces []string,
	logger logr.Logger,
) bool {
	if !r.HTTPRedirect {
		return false
	}
	for i := range gateway.Spec.Listeners {
		listener := &gateway.Spec.Listeners[i]
		if string(listener.Name) != httpListenerName {
			continue
		}
		if !isHTTPListener(*listener) {
			logger.Info("Gateway has an \"http\" listener not managed by ingress-doperator, leaving it",
				"gateway", gateway.Namespace+"/"+gateway.Name)
			return false
		}
		return r.updateListenerNamespaces(listener, namespaces)
	}
	if len(gateway.Spec.Listeners) >= MaxGatewayListeners {
		logListenerLimit(logger, gateway, httpListenerName)
		return false
	}
	listener := r.createListenerWithNamespaces("", namespaces, nil)
	listener.Name = httpListenerName
	listener.Hostname = nil
	listener.Port = httpListenerPort
	listener.Protocol = gatewayv1.HTTPProtocolType
	gateway.Spec.Listeners = append([]gatewayv1.Listener{listener}, gateway.Spec.Listeners...)
	logger.Info("Added HTTP listener", "gateway", gateway.Namespace+"/"+gateway.Name)
	return true
}

// addHTTPListenerNamespace adds namespace to the catch-all port 80 listener, creating it if needed.
func (r *HTTPRouteReconciler) addHTTPListenerNamespace(
	gateway *gatewayv1.Gateway,
	namespace string,
	logger logr.Logger,
) bool {
	for i := range gateway.Spec.Listeners {
		if isHTTPListener(gateway.Spec.Listeners[i]) {
			return r.addNamespaceToListener(&gateway.Spec.Listeners[i], namespace)
		}
	}
	namespaces := []string{r.GatewayNamespace}
	if namespace != r.GatewayNamespace {
		namespaces = append(namespaces, namespace)
		sort.Strings(namespaces)
	}
	return r.ensureHTTPListener(gateway, namespaces, logger)
}

// onlyHTTPListener reports whether the catch-all port 80 listener is the Gateway's only listener.
func onlyHTTPListener(gateway *gatewayv1.Gateway) bool {
	return len(gateway.Spec.Listeners) == 1 && isHTTPListener(gateway.Spec.Listeners[0])
}

// desiredHTTPSRedirectRoute returns the HTTPRoute that redirects every plain HTTP request on
// gateway's catch-all port 80 listener to HTTPS. It is owned by the Gateway, so it is deleted with
// it, and hidden from external-dns, as it names no hostnames.
func desiredHTTPSRedirectRoute(gateway *gatewayv1.Gateway) *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gateway.Name + httpsRedirectRouteSuffix,
			Namespace: gateway.Namespace,
			Annotations: map[string]string{
				HTTPSRedirectRouteAnnotation:     httpsRedirectRouteVersion,
				ExternalDNSGatewayHostnameSource: ExternalDNSHostnameSourceAnnotationOnly,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: gatewayv1.GroupVersion.String(),
				Kind:       gatewayKind,
				Name:       gateway.Name,
				UID:        gateway.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
				Name:        gatewayv1.ObjectName(gateway.Name),
				SectionName: ptr.To(gatewayv1.SectionName(httpListenerName)),
			}}},
			Rules: []gatewayv1.HTTPRouteRule{{
				Filters: []gatewayv1.HTTPRouteFilter{{
					Type: gatewayv1.HTTPRouteFilterRequestRedirect,
					RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
						Scheme:     ptr.To("https"),
						StatusCode: ptr.To(http.StatusMovedPermanently),
					},
				}},
			}},
		},
	}
}

// syncHTTPSRedirectRoute creates the redirect HTTPRoute of a Gateway that has the catch-all port 80
// listener, and updates it when its annotations (which carry its spec version) differ. A route of
// that name not managed by ingress-doperator is left alone.
func (r *HTTPRouteReconciler) syncHTTPSRedirectRoute(ctx context.Context, gateway *gatewayv1.Gateway) error {
	if !r.HTTPRedirect || gateway.UID == "" || !gatewayHasHTTPListener(gateway) {
		return nil
	}
	logger := log.FromContext(ctx)
	desired := desiredHTTPSRedirectRoute(gateway)
	existing := &gatewayv1.HTTPRoute{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		logger.Info("Creating HTTPS redirect HTTPRoute", "namespace", desired.Namespace, "name", desired.Name)
		return client.IgnoreAlreadyExists(r.Create(ctx, desired))
	}
	if err != nil {
		return fmt.Errorf("failed to get HTTPRoute %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	if !isHTTPSRedirectRoute(existing) {
		logger.Info("HTTPRoute exists and is not managed by ingress-doperator, leaving it",
			"namespace", existing.Namespace, "name", existing.Name)
		return nil
	}
	if maps.Equal(existing.Annotations, desired.Annotations) {
		return nil
	}
	existing.Annotations = desired.Annotations
	existing.OwnerReferences = desired.OwnerReferences
	existing.Spec = desired.Spec
	logger.Info("Updating HTTPS redirect HTTPRoute", "namespace", existing.Namespace, "name", existing.Name)
	return r.Update(ctx, existing)
}

func gatewayHasHTTPListener(gateway *gatewayv1.Gateway) bool {
	for _, listener := range gateway.Spec.Listeners {
		if isHTTPListener(listener) {
			return true
		}
	}
	return false
}

// syncGatewayCompanions brings the objects that accompany a written Gateway in line with it: the
// ReferenceGrants for wildcard certificates and the HTTPS redirect HTTPRoute.
func (r *HTTPRouteReconciler) syncGatewayCompanions(ctx context.Context, gateway *gatewayv1.Gateway) error {
	if err := r.syncWildcardReferenceGrants(ctx, gateway); err != nil {
		return err
	}
	return r.syncHTTPSRedirectRoute(ctx, gateway)
}
