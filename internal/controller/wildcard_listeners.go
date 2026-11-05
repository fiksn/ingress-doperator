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
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/translator"
	"github.com/fiksn/ingress-doperator/internal/utils"
)

// WildcardListener is one --wildcard-listeners entry: hostnames one label below Hostname's domain
// share a single listener for Hostname (e.g. *.review.example.com) that serves the certificate
// in Secret SecretNamespace/SecretName.
type WildcardListener struct {
	Hostname        string
	SecretNamespace string
	SecretName      string
}

// ParseWildcardListeners parses comma-separated '*.<domain>=<namespace>/<secret>' entries.
func ParseWildcardListeners(value string) ([]WildcardListener, error) {
	var listeners []WildcardListener
	seen := make(map[string]bool)
	for _, entry := range utils.ParseCommaSeparatedList(value) {
		listener, err := parseWildcardListener(entry)
		if err != nil {
			return nil, err
		}
		if seen[listener.Hostname] {
			return nil, fmt.Errorf("duplicate wildcard hostname %q", listener.Hostname)
		}
		seen[listener.Hostname] = true
		listeners = append(listeners, listener)
	}
	return listeners, nil
}

func parseWildcardListener(entry string) (WildcardListener, error) {
	hostname, secret, ok := strings.Cut(entry, "=")
	if !ok {
		return WildcardListener{}, fmt.Errorf("invalid entry %q: expected '*.<domain>=<namespace>/<secret>'", entry)
	}
	domain, ok := strings.CutPrefix(hostname, "*.")
	if !ok || len(validation.IsDNS1123Subdomain(domain)) > 0 {
		return WildcardListener{}, fmt.Errorf("invalid entry %q: hostname must be '*.' followed by a lowercase "+
			"DNS domain (e.g. '*.review.example.com')", entry)
	}
	namespace, name, ok := strings.Cut(secret, "/")
	if !ok || len(validation.IsDNS1123Label(namespace)) > 0 || len(validation.IsDNS1123Subdomain(name)) > 0 {
		return WildcardListener{}, fmt.Errorf("invalid entry %q: certificate must be '<namespace>/<secret>'", entry)
	}
	return WildcardListener{Hostname: hostname, SecretNamespace: namespace, SecretName: name}, nil
}

// matches reports whether host is exactly one label below the wildcard's domain, the hostnames
// a wildcard certificate covers. Gateway API would also match deeper hostnames.
func (w WildcardListener) matches(host string) bool {
	label, ok := strings.CutSuffix(host, strings.TrimPrefix(w.Hostname, "*"))
	return ok && label != "" && !strings.Contains(label, ".")
}

func (w WildcardListener) tlsConfig() *gatewayv1.ListenerTLSConfig {
	mode := gatewayv1.TLSModeTerminate
	namespace := gatewayv1.Namespace(w.SecretNamespace)
	return &gatewayv1.ListenerTLSConfig{
		Mode: &mode,
		CertificateRefs: []gatewayv1.SecretObjectReference{
			{Name: gatewayv1.ObjectName(w.SecretName), Namespace: &namespace},
		},
	}
}

// listenerHostname returns the hostname of the listener serving host: a matching wildcard, or
// host itself.
func listenerHostname(wildcards []WildcardListener, host string) string {
	for _, wildcard := range wildcards {
		if wildcard.matches(host) {
			return wildcard.Hostname
		}
	}
	return host
}

// listenerHostnames maps route hostnames to the hostnames of the listeners serving them, without
// duplicates and in order of first appearance.
func listenerHostnames(wildcards []WildcardListener, hosts []gatewayv1.Hostname) []gatewayv1.Hostname {
	seen := make(map[string]bool, len(hosts))
	out := make([]gatewayv1.Hostname, 0, len(hosts))
	for _, host := range hosts {
		listenerHost := listenerHostname(wildcards, string(host))
		if seen[listenerHost] {
			continue
		}
		seen[listenerHost] = true
		out = append(out, gatewayv1.Hostname(listenerHost))
	}
	return out
}

// wildcardTLS returns the TLS configuration of the wildcard listener for listenerHost, if it is one.
func wildcardTLS(wildcards []WildcardListener, listenerHost string) (*gatewayv1.ListenerTLSConfig, bool) {
	for _, wildcard := range wildcards {
		if wildcard.Hostname == listenerHost {
			return wildcard.tlsConfig(), true
		}
	}
	return nil, false
}

// applyWildcardSectionNames points the parentRefs of hostnames covered by a wildcard listener at
// that listener. The translator adds one parentRef per hostname; parentRefs must be unique, so
// hostnames sharing a wildcard listener collapse into one.
func applyWildcardSectionNames(httpRoute *gatewayv1.HTTPRoute, wildcards []WildcardListener) {
	if len(wildcards) == 0 {
		return
	}
	seen := make(map[string]bool, len(httpRoute.Spec.ParentRefs))
	refs := make([]gatewayv1.ParentReference, 0, len(httpRoute.Spec.ParentRefs))
	for _, ref := range httpRoute.Spec.ParentRefs {
		if ref.SectionName != nil {
			section := gatewayv1.SectionName(translator.ListenerNameForHostname(
				listenerHostname(wildcards, string(*ref.SectionName))))
			ref.SectionName = &section
		}
		key := parentRefKey(ref)
		if seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, ref)
	}
	httpRoute.Spec.ParentRefs = refs
}

// parentRefKey identifies the Gateway listener a parentRef points at.
func parentRefKey(ref gatewayv1.ParentReference) string {
	key := string(ref.Name)
	if ref.Namespace != nil {
		key = string(*ref.Namespace) + "/" + key
	}
	if ref.SectionName != nil {
		key += "#" + string(*ref.SectionName)
	}
	return key
}

// syncWildcardReferenceGrants keeps gateway listed as a source of the ReferenceGrant in the
// namespace of every wildcard certificate outside the Gateway namespace while the Gateway has that
// wildcard listener, and removes it otherwise, so the grant goes away with its last listener.
func (r *HTTPRouteReconciler) syncWildcardReferenceGrants(ctx context.Context, gateway *gatewayv1.Gateway) error {
	for _, wildcard := range r.WildcardListeners {
		if wildcard.SecretNamespace == gateway.Namespace {
			continue
		}
		source := fmt.Sprintf("gateway/%s/%s/%s",
			gateway.Namespace, gateway.Name, translator.ListenerNameForHostname(wildcard.Hostname))
		var err error
		if idx, _ := findListenerForHostname(gateway, wildcard.Hostname); idx >= 0 {
			err = r.ensureReferenceGrantSource(ctx, wildcard.SecretNamespace, source)
		} else {
			err = r.removeReferenceGrantSource(ctx, wildcard.SecretNamespace, source)
		}
		if err != nil {
			return fmt.Errorf("failed to sync ReferenceGrant in %s for listener %s of Gateway %s/%s: %w",
				wildcard.SecretNamespace, wildcard.Hostname, gateway.Namespace, gateway.Name, err)
		}
	}
	return nil
}
