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
	"crypto/sha256"
	"encoding/hex"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// maxObjectNameLength is the Kubernetes limit for names that are DNS subdomains.
const maxObjectNameLength = 253

// hostGroup is a set of Ingress hosts served by the same paths.
type hostGroup struct {
	hosts []string
	paths []networkingv1.HTTPIngressPath
}

// TranslateToHTTPRoutesWithCanaries translates the Ingress into one HTTPRoute per group of hosts
// with identical paths. Ingress rules apply to their own host only, while the rules of an
// HTTPRoute apply to all of its hostnames, so hosts with different paths or backends need routes
// of their own. When every host has the same paths this is the single route of
// TranslateToHTTPRouteWithCanaries. The first group's route keeps the Ingress's name; the others
// are named after their first host.
func (t *Translator) TranslateToHTTPRoutesWithCanaries(
	ingress *networkingv1.Ingress,
	canaries []*networkingv1.Ingress,
) []*gatewayv1.HTTPRoute {
	groups := hostGroups(ingress)
	if len(groups) <= 1 {
		return []*gatewayv1.HTTPRoute{t.TranslateToHTTPRouteWithCanaries(ingress, canaries)}
	}
	routes := make([]*gatewayv1.HTTPRoute, 0, len(groups))
	for i, group := range groups {
		route := t.TranslateToHTTPRouteWithCanaries(group.ingress(ingress), canaries)
		if i > 0 {
			route.Name = hostGroupRouteName(ingress.Name, group.hosts[0])
		}
		routes = append(routes, route)
	}
	return routes
}

// hostGroups groups the Ingress's hosts by the paths serving them, in order of first appearance.
// Paths of rules without a host apply to every host, after the host's own paths; a host listed in
// several rules gets the paths of all of them.
func hostGroups(ingress *networkingv1.Ingress) []hostGroup {
	var shared []networkingv1.HTTPIngressPath
	pathsByHost := make(map[string][]networkingv1.HTTPIngressPath)
	var hosts []string
	for _, rule := range ingress.Spec.Rules {
		var paths []networkingv1.HTTPIngressPath
		if rule.HTTP != nil {
			paths = rule.HTTP.Paths
		}
		if rule.Host == "" {
			shared = append(shared, paths...)
			continue
		}
		if _, seen := pathsByHost[rule.Host]; !seen {
			hosts = append(hosts, rule.Host)
		}
		pathsByHost[rule.Host] = append(pathsByHost[rule.Host], paths...)
	}

	var groups []hostGroup
	for _, host := range hosts {
		paths := append(append([]networkingv1.HTTPIngressPath{}, pathsByHost[host]...), shared...)
		if i := findHostGroup(groups, paths); i >= 0 {
			groups[i].hosts = append(groups[i].hosts, host)
			continue
		}
		groups = append(groups, hostGroup{hosts: []string{host}, paths: paths})
	}
	return groups
}

func findHostGroup(groups []hostGroup, paths []networkingv1.HTTPIngressPath) int {
	for i := range groups {
		if equality.Semantic.DeepEqual(groups[i].paths, paths) {
			return i
		}
	}
	return -1
}

// ingress returns a copy of ingress with only the group's hosts and paths. The paths are listed
// once, under the first host, so the translated route has them once.
func (g hostGroup) ingress(ingress *networkingv1.Ingress) *networkingv1.Ingress {
	grouped := ingress.DeepCopy()
	rules := make([]networkingv1.IngressRule, 0, len(g.hosts))
	for i, host := range g.hosts {
		rule := networkingv1.IngressRule{Host: host}
		if i == 0 {
			rule.HTTP = &networkingv1.HTTPIngressRuleValue{Paths: g.paths}
		}
		rules = append(rules, rule)
	}
	grouped.Spec.Rules = rules
	return grouped
}

// hostGroupRouteName names the HTTPRoute of a host group after the Ingress and the group's first
// host, shortened with a hash when it would exceed the Kubernetes name limit. It starts with the
// Ingress name so the route is found and cleaned up with the Ingress's other routes.
func hostGroupRouteName(ingressName, host string) string {
	name := ingressName + "-" + strings.ToLower(strings.ReplaceAll(host, "*", "wildcard"))
	if len(name) <= maxObjectNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:])[:10]
	return strings.TrimRight(name[:maxObjectNameLength-len(suffix)], "-.") + suffix
}
