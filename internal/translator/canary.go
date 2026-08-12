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
	"regexp"
	"strconv"

	"github.com/go-logr/logr"
	networkingv1 "k8s.io/api/networking/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ingress-nginx canary annotations.
const (
	CanaryAnnotation                = "nginx.ingress.kubernetes.io/canary"
	CanaryWeightAnnotation          = "nginx.ingress.kubernetes.io/canary-weight"
	CanaryWeightTotalAnnotation     = "nginx.ingress.kubernetes.io/canary-weight-total"
	CanaryByHeaderAnnotation        = "nginx.ingress.kubernetes.io/canary-by-header"
	CanaryByHeaderValueAnnotation   = "nginx.ingress.kubernetes.io/canary-by-header-value"
	CanaryByHeaderPatternAnnotation = "nginx.ingress.kubernetes.io/canary-by-header-pattern"
	CanaryByCookieAnnotation        = "nginx.ingress.kubernetes.io/canary-by-cookie"

	canaryOnValue  = "always"
	canaryOffValue = "never"
	cookieHeader   = "Cookie"
)

// CanaryConfig holds the parsed ingress-nginx canary settings from a canary Ingress.
type CanaryConfig struct {
	Weight        int32
	WeightTotal   int32
	Header        string
	HeaderValue   string
	HeaderPattern string
	Cookie        string
}

// IsCanary reports whether the Ingress carries the ingress-nginx canary annotation.
func IsCanary(ingress *networkingv1.Ingress) bool {
	return ingress != nil && ingress.Annotations[CanaryAnnotation] == "true"
}

// ParseCanaryConfig extracts canary configuration from a canary Ingress. Weight
// defaults to 0 (no traffic) and weight-total defaults to 100, matching ingress-nginx.
func ParseCanaryConfig(ingress *networkingv1.Ingress) (CanaryConfig, error) {
	config := CanaryConfig{Weight: 0, WeightTotal: 100}
	annotations := ingress.Annotations

	if raw := annotations[CanaryWeightAnnotation]; raw != "" {
		weight, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || weight < 0 {
			return config, fmt.Errorf("invalid %s %q: must be a non-negative integer", CanaryWeightAnnotation, raw)
		}
		config.Weight = int32(weight)
	}
	if raw := annotations[CanaryWeightTotalAnnotation]; raw != "" {
		total, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || total <= 0 {
			return config, fmt.Errorf("invalid %s %q: must be a positive integer", CanaryWeightTotalAnnotation, raw)
		}
		config.WeightTotal = int32(total)
	}
	if config.Weight > config.WeightTotal {
		return config, fmt.Errorf("%s (%d) exceeds %s (%d)",
			CanaryWeightAnnotation, config.Weight, CanaryWeightTotalAnnotation, config.WeightTotal)
	}

	config.Header = annotations[CanaryByHeaderAnnotation]
	config.HeaderValue = annotations[CanaryByHeaderValueAnnotation]
	config.HeaderPattern = annotations[CanaryByHeaderPatternAnnotation]
	config.Cookie = annotations[CanaryByCookieAnnotation]
	return config, nil
}

// canaryEntry pairs a parsed canary configuration with the backend that serves it.
type canaryEntry struct {
	config     CanaryConfig
	backendRef gatewayv1.HTTPBackendRef
}

// collectCanaryEntries maps each request path to the canary backend serving it.
// Canary Ingresses with invalid configuration are skipped with a logged error.
func collectCanaryEntries(canaries []*networkingv1.Ingress, logger logr.Logger) map[string]canaryEntry {
	entries := make(map[string]canaryEntry)
	for _, canary := range canaries {
		config, err := ParseCanaryConfig(canary)
		if err != nil {
			logger.Error(err, "skipping canary Ingress with invalid configuration",
				"namespace", canary.Namespace, "name", canary.Name)
			continue
		}
		for _, rule := range canary.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for _, path := range rule.HTTP.Paths {
				ref, ok := backendRefFromPath(path)
				if !ok {
					continue
				}
				if _, exists := entries[path.Path]; exists {
					logger.Info("multiple canary backends target the same path; keeping the last one",
						"namespace", canary.Namespace, "name", canary.Name, "path", path.Path)
				}
				entries[path.Path] = canaryEntry{config: config, backendRef: ref}
			}
		}
	}
	return entries
}

// backendRefFromPath builds an HTTPBackendRef from an Ingress path's service backend.
// The second return value is false when the path has no service backend.
func backendRefFromPath(path networkingv1.HTTPIngressPath) (gatewayv1.HTTPBackendRef, bool) {
	if path.Backend.Service == nil {
		return gatewayv1.HTTPBackendRef{}, false
	}
	ref := gatewayv1.HTTPBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(path.Backend.Service.Name),
			},
		},
	}
	if path.Backend.Service.Port.Number > 0 {
		port := path.Backend.Service.Port.Number
		ref.Port = &port
	} else if path.Backend.Service.Port.Name != "" {
		// Named port - resolved later by the controller against the Service.
		port := int32(0)
		ref.Port = &port
	}
	return ref, true
}

// buildCanaryRules produces the HTTPRoute rules for a single path that has a canary
// backend. It honours ingress-nginx precedence (header > cookie > weight): header and
// cookie matches are emitted as higher-specificity rules that win over the weight split.
func buildCanaryRules(
	base gatewayv1.HTTPRouteMatch,
	hasPath bool,
	primaryRef gatewayv1.HTTPBackendRef,
	entry canaryEntry,
	filters []gatewayv1.HTTPRouteFilter,
) []gatewayv1.HTTPRouteRule {
	config := entry.config
	rules := make([]gatewayv1.HTTPRouteRule, 0, 4)

	// In default header mode (no explicit value/pattern), "never" forces the primary.
	if config.Header != "" && config.HeaderValue == "" && config.HeaderPattern == "" {
		rules = append(rules, backendRule(
			overrideMatches(base, config.Header, gatewayv1.HeaderMatchExact, canaryOffValue),
			primaryRef, filters))
	}
	if config.Header != "" {
		matchType, value := canaryHeaderMatch(config)
		rules = append(rules, backendRule(
			overrideMatches(base, config.Header, matchType, value),
			canaryOnlyRef(entry.backendRef), filters))
	}
	if config.Cookie != "" {
		rules = append(rules, backendRule(
			overrideMatches(base, cookieHeader, gatewayv1.HeaderMatchRegularExpression, cookieOnPattern(config.Cookie)),
			canaryOnlyRef(entry.backendRef), filters))
	}

	rules = append(rules, weightedRule(baseMatches(base, hasPath), primaryRef, entry, filters))
	return rules
}

// canaryHeaderMatch resolves the header match type and value for the canary rule.
// A pattern takes precedence over an explicit value, which takes precedence over "always".
func canaryHeaderMatch(config CanaryConfig) (gatewayv1.HeaderMatchType, string) {
	switch {
	case config.HeaderPattern != "":
		return gatewayv1.HeaderMatchRegularExpression, config.HeaderPattern
	case config.HeaderValue != "":
		return gatewayv1.HeaderMatchExact, config.HeaderValue
	default:
		return gatewayv1.HeaderMatchExact, canaryOnValue
	}
}

// cookieOnPattern builds a RE2 pattern matching a "<name>=always" cookie anywhere in
// the Cookie header value.
func cookieOnPattern(name string) string {
	return `(?:^|;\s*)` + regexp.QuoteMeta(name) + `=` + canaryOnValue + `(?:;|$)`
}

// baseMatches returns the path match wrapped in a slice, or nil when there is no path.
func baseMatches(base gatewayv1.HTTPRouteMatch, hasPath bool) []gatewayv1.HTTPRouteMatch {
	if !hasPath {
		return nil
	}
	return []gatewayv1.HTTPRouteMatch{base}
}

// overrideMatches clones the base match and appends a header match, producing a
// higher-specificity match for header/cookie based canary routing.
func overrideMatches(
	base gatewayv1.HTTPRouteMatch,
	name string,
	matchType gatewayv1.HeaderMatchType,
	value string,
) []gatewayv1.HTTPRouteMatch {
	headerType := matchType
	headers := make([]gatewayv1.HTTPHeaderMatch, len(base.Headers), len(base.Headers)+1)
	copy(headers, base.Headers)
	headers = append(headers, gatewayv1.HTTPHeaderMatch{
		Type:  &headerType,
		Name:  gatewayv1.HTTPHeaderName(name),
		Value: value,
	})
	match := base
	match.Headers = headers
	return []gatewayv1.HTTPRouteMatch{match}
}

// backendRule builds a rule routing all matched traffic to a single backend.
func backendRule(
	matches []gatewayv1.HTTPRouteMatch,
	ref gatewayv1.HTTPBackendRef,
	filters []gatewayv1.HTTPRouteFilter,
) gatewayv1.HTTPRouteRule {
	return gatewayv1.HTTPRouteRule{
		Matches:     matches,
		BackendRefs: []gatewayv1.HTTPBackendRef{ref},
		Filters:     filters,
	}
}

// weightedRule builds the weight-split rule between the primary and canary backends.
func weightedRule(
	matches []gatewayv1.HTTPRouteMatch,
	primaryRef gatewayv1.HTTPBackendRef,
	entry canaryEntry,
	filters []gatewayv1.HTTPRouteFilter,
) gatewayv1.HTTPRouteRule {
	primary := primaryRef
	canary := entry.backendRef
	canaryWeight := entry.config.Weight
	primaryWeight := entry.config.WeightTotal - canaryWeight
	primary.Weight = &primaryWeight
	canary.Weight = &canaryWeight
	return gatewayv1.HTTPRouteRule{
		Matches:     matches,
		BackendRefs: []gatewayv1.HTTPBackendRef{primary, canary},
		Filters:     filters,
	}
}

// canaryOnlyRef returns the canary backend with no weight so it receives all matched traffic.
func canaryOnlyRef(ref gatewayv1.HTTPBackendRef) gatewayv1.HTTPBackendRef {
	ref.Weight = nil
	return ref
}
