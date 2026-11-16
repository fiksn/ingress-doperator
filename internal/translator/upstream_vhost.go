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
	"regexp"
	"strings"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// UpstreamVhostAnnotation sets the Host header ingress-nginx sends to the backends.
const UpstreamVhostAnnotation = "nginx.ingress.kubernetes.io/upstream-vhost"

// preciseHostnameRe is the format of a Gateway API PreciseHostname, which a URLRewrite filter's
// hostname must have: no wildcard, port or NGINX variable.
var preciseHostnameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

const maxPreciseHostnameLength = 253

// upstreamVhost returns the upstream-vhost annotation's hostname, lowercased, and whether the
// annotation is set and whether a URLRewrite filter can express its value.
func upstreamVhost(annotations map[string]string) (hostname string, set, translatable bool) {
	raw, set := annotations[UpstreamVhostAnnotation]
	hostname = strings.ToLower(strings.TrimSpace(raw))
	if !set || hostname == "" {
		return "", false, false
	}
	return hostname, true, len(hostname) <= maxPreciseHostnameLength && preciseHostnameRe.MatchString(hostname)
}

// buildUpstreamVhostFilter returns the URLRewrite filter that sets the Host header sent to the
// backends to the upstream-vhost annotation's hostname, or nil when the annotation is not set or
// its value cannot be expressed (see UnsupportedAnnotationWarnings).
func buildUpstreamVhostFilter(annotations map[string]string) *gatewayv1.HTTPRouteFilter {
	hostname, _, translatable := upstreamVhost(annotations)
	if !translatable {
		return nil
	}
	precise := gatewayv1.PreciseHostname(hostname)
	return &gatewayv1.HTTPRouteFilter{
		Type:       gatewayv1.HTTPRouteFilterURLRewrite,
		URLRewrite: &gatewayv1.HTTPURLRewriteFilter{Hostname: &precise},
	}
}
