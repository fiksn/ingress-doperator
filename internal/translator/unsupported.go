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

package translator

import (
	"fmt"
	"sort"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
)

// BackendProtocolAnnotation selects the protocol ingress-nginx uses towards the backends.
const BackendProtocolAnnotation = "nginx.ingress.kubernetes.io/backend-protocol"

// UnsupportedAnnotationWarnings explains the Ingress annotations whose behavior the generated
// HTTPRoutes do not reproduce, so the user can configure it in Gateway API.
func UnsupportedAnnotationWarnings(ingress *networkingv1.Ingress) []string {
	var warnings []string
	// HTTPS is translated into a BackendTLSPolicy by the operator (see BackendTLSFor).
	protocol := strings.TrimSpace(ingress.Annotations[BackendProtocolAnnotation])
	if protocol != "" && !strings.EqualFold(protocol, "HTTP") && !UsesBackendTLS(ingress) {
		warnings = append(warnings, backendProtocolWarning(ingress, protocol))
	}
	if _, set, translatable := upstreamVhost(ingress.Annotations); set && !translatable {
		warnings = append(warnings, fmt.Sprintf("%s: %q is not translated: only a plain hostname "+
			"(no NGINX variables, port or wildcard) can become the HTTPRoute's URLRewrite hostname, "+
			"so the backends receive the original Host header.",
			UpstreamVhostAnnotation, ingress.Annotations[UpstreamVhostAnnotation]))
	}
	return warnings
}

func backendProtocolWarning(ingress *networkingv1.Ingress, protocol string) string {
	return fmt.Sprintf("%s: %s is not translated: the HTTPRoute sends plain HTTP to Service(s) %s.",
		BackendProtocolAnnotation, protocol, strings.Join(backendServiceNames(ingress), ", "))
}

// backendServiceNames returns the sorted, distinct names of the Services the Ingress routes to.
func backendServiceNames(ingress *networkingv1.Ingress) []string {
	seen := make(map[string]bool)
	add := func(backend *networkingv1.IngressBackend) {
		if backend != nil && backend.Service != nil {
			seen[backend.Service.Name] = true
		}
	}
	add(ingress.Spec.DefaultBackend)
	for _, rule := range ingress.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for i := range rule.HTTP.Paths {
			add(&rule.HTTP.Paths[i].Backend)
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
