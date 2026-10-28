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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strconv"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/fiksn/ingress-doperator/internal/utils"
)

// ParseOneGatewayPerIngress converts the --one-gateway-per-ingress value into namespace/name glob
// patterns. Boolean values are accepted for convenience: true selects every Ingress ("*/*"),
// false (or an empty value) selects none.
func ParseOneGatewayPerIngress(value string) ([]string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, nil
	}
	if enabled, err := strconv.ParseBool(trimmed); err == nil {
		if enabled {
			return []string{"*/*"}, nil
		}
		return nil, nil
	}

	patterns := utils.ParseCommaSeparatedList(trimmed)
	for _, pattern := range patterns {
		if strings.Count(pattern, "/") != 1 {
			return nil, fmt.Errorf("invalid pattern %q: expected namespace/name (e.g. 'prod/api-*', '*/*')", pattern)
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
		}
	}
	return patterns, nil
}

// wantsDedicatedGateway reports whether the Ingress matches a --one-gateway-per-ingress pattern.
func (r *IngressReconciler) wantsDedicatedGateway(ingress *networkingv1.Ingress) bool {
	key := ingress.Namespace + "/" + ingress.Name
	for _, pattern := range r.OneGatewayPerIngress {
		// Patterns are validated at startup, so Match cannot fail here.
		if matched, _ := path.Match(pattern, key); matched {
			return true
		}
	}
	return false
}

// gatewayNameFor returns the Gateway an Ingress's HTTPRoutes attach to: a dedicated Gateway
// for Ingresses selected by --one-gateway-per-ingress, otherwise the shared Gateway for the
// Ingress's class.
func (r *IngressReconciler) gatewayNameFor(ingress *networkingv1.Ingress) string {
	if r.wantsDedicatedGateway(ingress) {
		return dedicatedGatewayName(ingress.Namespace, ingress.Name)
	}
	return r.getGatewayNameForClass(r.getIngressClass(ingress))
}

// maxDedicatedGatewayNameLength keeps dedicated Gateway names valid DNS labels; Gateway
// controllers commonly derive Service names (limited to 63 characters) from the Gateway name.
const maxDedicatedGatewayNameLength = 63

const dedicatedGatewayHashLength = 8

// dedicatedGatewayName returns <namespace>-<name>-<hash>, where hash is derived from
// "namespace/name". The hash keeps names unique where joining with "-" alone is ambiguous
// (a-b/c vs a/b-c) and when the readable part is truncated to fit the length limit.
func dedicatedGatewayName(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	suffix := "-" + hex.EncodeToString(sum[:])[:dedicatedGatewayHashLength]

	readable := strings.ReplaceAll(namespace+"-"+name, ".", "-")
	if limit := maxDedicatedGatewayNameLength - len(suffix); len(readable) > limit {
		readable = readable[:limit]
	}
	return strings.TrimRight(readable, "-") + suffix
}
