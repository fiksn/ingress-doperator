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
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// BackendTLSCAAnnotation names the CA bundle (key ca.crt) of the BackendTLSPolicy generated for
	// backend-protocol: HTTPS, as "Secret/<name>" or "ConfigMap/<name>" in the Ingress namespace.
	BackendTLSCAAnnotation   = "ingress-doperator.fiction.si/backend-tls-ca"
	ProxySSLNameAnnotation   = "nginx.ingress.kubernetes.io/proxy-ssl-name"
	ProxySSLSecretAnnotation = "nginx.ingress.kubernetes.io/proxy-ssl-secret"
	ProxySSLVerifyAnnotation = "nginx.ingress.kubernetes.io/proxy-ssl-verify"

	caKindSecret    = "Secret"
	caKindConfigMap = "ConfigMap"
)

// BackendTLS is the backend TLS an Ingress asks for with backend-protocol: HTTPS, as a
// BackendTLSPolicy can express it.
type BackendTLS struct {
	// Hostname is the SNI and certificate name; empty means the Service's cluster DNS name.
	Hostname string
	// CAKind and CAName reference the CA bundle; empty CAKind means the well-known system CAs.
	CAKind string
	CAName string
}

// UsesBackendTLS reports whether the Ingress asks for TLS to its backends.
func UsesBackendTLS(ingress *networkingv1.Ingress) bool {
	return strings.EqualFold(strings.TrimSpace(ingress.Annotations[BackendProtocolAnnotation]), "HTTPS")
}

// BackendTLSFor returns the backend TLS settings of an Ingress using backend-protocol: HTTPS, with
// warnings for the settings a BackendTLSPolicy cannot express. The hostname comes from
// proxy-ssl-name, else upstream-vhost; the CA from backend-tls-ca, else proxy-ssl-secret, else the
// system CAs, against which self-signed backend certificates fail.
func BackendTLSFor(ingress *networkingv1.Ingress) (BackendTLS, []string) {
	var tls BackendTLS
	var warnings []string
	tls.Hostname, warnings = backendTLSHostname(ingress.Annotations)

	caKind, caName, caWarnings := backendTLSCA(ingress)
	tls.CAKind, tls.CAName = caKind, caName
	warnings = append(warnings, caWarnings...)

	if strings.EqualFold(strings.TrimSpace(ingress.Annotations[ProxySSLVerifyAnnotation]), "off") {
		warnings = append(warnings, fmt.Sprintf("%s: off cannot be expressed: the BackendTLSPolicy always "+
			"verifies the backend certificate.", ProxySSLVerifyAnnotation))
	}
	return tls, warnings
}

func backendTLSHostname(annotations map[string]string) (string, []string) {
	if raw, set := annotations[ProxySSLNameAnnotation]; set && strings.TrimSpace(raw) != "" {
		hostname := strings.ToLower(strings.TrimSpace(raw))
		if len(hostname) <= maxPreciseHostnameLength && preciseHostnameRe.MatchString(hostname) {
			return hostname, nil
		}
		return "", []string{fmt.Sprintf("%s: %q is not a plain hostname; using the Service's DNS name "+
			"for the BackendTLSPolicy.", ProxySSLNameAnnotation, raw)}
	}
	if hostname, _, translatable := upstreamVhost(annotations); translatable {
		return hostname, nil
	}
	return "", nil
}

func backendTLSCA(ingress *networkingv1.Ingress) (kind, name string, warnings []string) {
	if raw := strings.TrimSpace(ingress.Annotations[BackendTLSCAAnnotation]); raw != "" {
		kind, name, ok := parseCARef(raw)
		if ok {
			return kind, name, nil
		}
		warnings = append(warnings, fmt.Sprintf("%s: %q is not \"Secret/<name>\" or \"ConfigMap/<name>\".",
			BackendTLSCAAnnotation, raw))
	} else if raw := strings.TrimSpace(ingress.Annotations[ProxySSLSecretAnnotation]); raw != "" {
		name, ok, warning := proxySSLSecretName(ingress.Namespace, raw)
		warnings = append(warnings, fmt.Sprintf("%s: only its ca.crt is used; a client certificate "+
			"(tls.crt/tls.key) in it is not sent, as a BackendTLSPolicy cannot present one.",
			ProxySSLSecretAnnotation))
		if ok {
			return caKindSecret, name, warnings
		}
		warnings = append(warnings, warning)
	}
	warnings = append(warnings, fmt.Sprintf("No usable CA for the backend certificate: the BackendTLSPolicy "+
		"uses the system CAs, so self-signed backend certificates fail. Set %s.", BackendTLSCAAnnotation))
	return "", "", warnings
}

// parseCARef parses "Secret/<name>" or "ConfigMap/<name>" (kind case-insensitive).
func parseCARef(raw string) (kind, name string, ok bool) {
	kindPart, name, found := strings.Cut(raw, "/")
	if !found || len(validation.IsDNS1123Subdomain(name)) > 0 {
		return "", "", false
	}
	switch {
	case strings.EqualFold(kindPart, caKindSecret):
		return caKindSecret, name, true
	case strings.EqualFold(kindPart, caKindConfigMap):
		return caKindConfigMap, name, true
	default:
		return "", "", false
	}
}

// proxySSLSecretName returns the Secret name of proxy-ssl-secret ("<namespace>/<name>" or
// "<name>"), which must be in the Ingress namespace: a BackendTLSPolicy only references CAs in its
// own namespace.
func proxySSLSecretName(namespace, raw string) (name string, ok bool, warning string) {
	secretNamespace, name, found := strings.Cut(raw, "/")
	if !found {
		secretNamespace, name = namespace, raw
	}
	if secretNamespace != namespace {
		return "", false, fmt.Sprintf("%s: Secret %q is not in namespace %s, which a BackendTLSPolicy "+
			"cannot reference.", ProxySSLSecretAnnotation, raw, namespace)
	}
	if len(validation.IsDNS1123Subdomain(name)) > 0 {
		return "", false, fmt.Sprintf("%s: %q is not a valid Secret name.", ProxySSLSecretAnnotation, raw)
	}
	return name, true, ""
}

// BackendServiceNames returns the sorted, distinct names of the Services the Ingress routes to.
func BackendServiceNames(ingress *networkingv1.Ingress) []string {
	return backendServiceNames(ingress)
}
