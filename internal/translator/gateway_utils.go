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
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// MergeGatewaySpec merges the desired Gateway spec into the existing Gateway
// Listeners are merged by hostname (unique by listener name)
// Annotations are merged (desired overwrites existing on conflict, except for special cases)
func MergeGatewaySpec(existing, desired *gatewayv1.Gateway) {
	// Merge annotations
	if existing.Annotations == nil {
		existing.Annotations = make(map[string]string)
	}
	for k, v := range desired.Annotations {
		// Special handling for certificate-mismatch annotation - merge with semicolon separator
		if k == MismatchedCertAnnotation {
			existing.Annotations[k] = MergeCertificateMismatchAnnotation(
				existing.Annotations[k], v)
		} else if k == SourceAnnotation {
			existing.Annotations[k] = MergeAnnotationValues(
				existing.Annotations[k], v)
		} else if strings.HasPrefix(k, "ingress-doperator.fiction.si/") {
			// Other ingress-doperator annotations are not merged, just overwrite
			existing.Annotations[k] = v
		} else {
			// General annotations from Ingress resources - merge with comma separator
			existing.Annotations[k] = MergeAnnotationValues(
				existing.Annotations[k], v)
		}
	}

	// Update GatewayClassName
	existing.Spec.GatewayClassName = desired.Spec.GatewayClassName

	// Merge Infrastructure annotations
	if desired.Spec.Infrastructure != nil {
		if existing.Spec.Infrastructure == nil {
			existing.Spec.Infrastructure = &gatewayv1.GatewayInfrastructure{
				Annotations: make(map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue),
			}
		}
		if existing.Spec.Infrastructure.Annotations == nil {
			existing.Spec.Infrastructure.Annotations = make(map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue)
		}
		for k, v := range desired.Spec.Infrastructure.Annotations {
			existing.Spec.Infrastructure.Annotations[k] = v
		}
	}

	// Merge listeners - use listener name as unique key
	// Build map of existing listeners by name
	existingListeners := make(map[gatewayv1.SectionName]gatewayv1.Listener)
	for _, listener := range existing.Spec.Listeners {
		existingListeners[listener.Name] = listener
	}

	// Add or update listeners from desired
	for _, desiredListener := range desired.Spec.Listeners {
		existingListeners[desiredListener.Name] = desiredListener
	}

	// Convert map back to slice
	mergedListeners := make([]gatewayv1.Listener, 0, len(existingListeners))
	for _, listener := range existingListeners {
		mergedListeners = append(mergedListeners, listener)
	}

	existing.Spec.Listeners = mergedListeners
}

// RemoveIngressListeners removes listeners from the Gateway that correspond to hostnames in the Ingress
// Also cleans up certificate-mismatch annotation entries for removed hostnames
func RemoveIngressListeners(gateway *gatewayv1.Gateway, ingress *networkingv1.Ingress, t *Translator) {
	// Build set of hostnames from this Ingress (with transformation applied)
	hostnamesToRemove := make(map[string]bool)
	originalToTransformed := make(map[string]string)
	for _, rule := range ingress.Spec.Rules {
		if rule.Host != "" {
			transformedHostname := t.TransformHostname(rule.Host)
			hostnamesToRemove[transformedHostname] = true
			originalToTransformed[rule.Host] = transformedHostname
		}
	}

	// Filter out listeners matching these hostnames
	remainingListeners := make([]gatewayv1.Listener, 0)
	for _, listener := range gateway.Spec.Listeners {
		// Listener name is the hostname (as per synthesizeSharedGateway)
		listenerHostname := string(listener.Name)
		if !hostnamesToRemove[listenerHostname] {
			remainingListeners = append(remainingListeners, listener)
		}
	}

	gateway.Spec.Listeners = remainingListeners

	// Clean up certificate-mismatch annotation entries for removed hostnames
	if gateway.Annotations != nil {
		if certMismatch, exists := gateway.Annotations[MismatchedCertAnnotation]; exists {
			gateway.Annotations[MismatchedCertAnnotation] =
				RemoveCertMismatchEntries(certMismatch, originalToTransformed)
		}
	}
}

// MergeCertificateMismatchAnnotation merges certificate mismatch values from multiple reconciliations
// Values are semicolon-separated, and we deduplicate them
func MergeCertificateMismatchAnnotation(existing, desired string) string {
	if existing == "" {
		return desired
	}
	if desired == "" {
		return existing
	}

	// Split both by semicolon and collect unique values
	valuesMap := make(map[string]bool)

	// Add existing values
	for _, val := range strings.Split(existing, ";") {
		trimmed := strings.TrimSpace(val)
		if trimmed != "" {
			valuesMap[trimmed] = true
		}
	}

	// Add desired values
	for _, val := range strings.Split(desired, ";") {
		trimmed := strings.TrimSpace(val)
		if trimmed != "" {
			valuesMap[trimmed] = true
		}
	}

	// Convert back to slice and sort for consistent ordering
	values := make([]string, 0, len(valuesMap))
	for val := range valuesMap {
		values = append(values, val)
	}

	// Join with semicolon-space separator
	return strings.Join(values, "; ")
}

// MergeAnnotationValues merges annotation values from multiple Ingresses
// Values are comma-separated, and we deduplicate them
func MergeAnnotationValues(existing, desired string) string {
	if existing == "" {
		return desired
	}
	if desired == "" {
		return existing
	}

	// Split both by comma and collect unique values
	valuesMap := make(map[string]bool)

	// Add existing values
	for _, val := range strings.Split(existing, ",") {
		trimmed := strings.TrimSpace(val)
		if trimmed != "" {
			valuesMap[trimmed] = true
		}
	}

	// Add desired values
	for _, val := range strings.Split(desired, ",") {
		trimmed := strings.TrimSpace(val)
		if trimmed != "" {
			valuesMap[trimmed] = true
		}
	}

	// Convert back to slice (maintain insertion order by re-parsing)
	// We'll preserve the order by checking which values appear first
	var values []string
	seenInResult := make(map[string]bool)

	// Add from existing first (preserves original order)
	for _, val := range strings.Split(existing, ",") {
		trimmed := strings.TrimSpace(val)
		if trimmed != "" && !seenInResult[trimmed] {
			values = append(values, trimmed)
			seenInResult[trimmed] = true
		}
	}

	// Add from desired second (new values appear at end)
	for _, val := range strings.Split(desired, ",") {
		trimmed := strings.TrimSpace(val)
		if trimmed != "" && !seenInResult[trimmed] {
			values = append(values, trimmed)
			seenInResult[trimmed] = true
		}
	}

	// Join with comma separator
	return strings.Join(values, ",")
}

// RemoveCertMismatchEntries removes certificate mismatch entries that match the given hostname mappings
// Format: "original->transformed: namespace/secret->namespace/newsecret"
func RemoveCertMismatchEntries(
	certMismatch string, hostnameMappings map[string]string,
) string {
	if certMismatch == "" {
		return ""
	}

	// Split by semicolon
	entries := strings.Split(certMismatch, ";")
	remainingEntries := make([]string, 0, len(entries))

	for _, entry := range entries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}

		// Check if this entry matches any hostname mapping to remove
		// Format: "original->transformed: ..."
		shouldKeep := true
		for original, transformed := range hostnameMappings {
			prefix := original + "->" + transformed + ":"
			if strings.HasPrefix(trimmed, prefix) {
				shouldKeep = false
				break
			}
		}

		if shouldKeep {
			remainingEntries = append(remainingEntries, trimmed)
		}
	}

	if len(remainingEntries) == 0 {
		return ""
	}

	return strings.Join(remainingEntries, "; ")
}
