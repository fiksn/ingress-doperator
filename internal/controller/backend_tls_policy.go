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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/translator"
)

const (
	backendTLSPolicyPrefix = "ingress-doperator-"
	serviceKind            = "Service"
)

// backendTLSPolicyName names the BackendTLSPolicy ingress-doperator manages for a Service, shortened
// with a hash when it would exceed the Kubernetes name limit.
func backendTLSPolicyName(service string) string {
	name := backendTLSPolicyPrefix + service
	if len(name) <= 253 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:])[:10]
	return strings.TrimRight(name[:253-len(suffix)], "-.") + suffix
}

// reconcileBackendTLSPolicies keeps the BackendTLSPolicies of the Services the Ingress routes to in
// line with backend-protocol: HTTPS, and releases the policies of Services it no longer needs. A
// policy is shared by all Ingresses using the Service (listed in its source annotation) and is
// deleted once none needs it.
func (r *IngressReconciler) reconcileBackendTLSPolicies(ctx context.Context, ingress *networkingv1.Ingress) error {
	policies := &gatewayv1.BackendTLSPolicyList{}
	if err := r.List(ctx, policies, client.InNamespace(ingress.Namespace)); err != nil {
		if isNoMatchError(err) {
			if translator.UsesBackendTLS(ingress) {
				r.recordWarning(ingress, "BackendTLSPolicyUnavailable",
					"backend-protocol: HTTPS needs a BackendTLSPolicy, but the BackendTLSPolicy CRD is not installed")
			}
			return nil
		}
		return fmt.Errorf("failed to list BackendTLSPolicies in %s: %w", ingress.Namespace, err)
	}

	services := map[string]bool{}
	if translator.UsesBackendTLS(ingress) && ingress.DeletionTimestamp.IsZero() {
		for _, service := range translator.BackendServiceNames(ingress) {
			services[service] = true
		}
	}
	ingressKey := ingress.Namespace + "/" + ingress.Name
	for i := range policies.Items {
		policy := &policies.Items[i]
		if isManagedBackendTLSPolicy(policy) && slices.Contains(policySources(policy), ingressKey) {
			if service := managedPolicyService(policy); service != "" {
				services[service] = true
			}
		}
	}

	for _, service := range sortedKeys(services) {
		if err := r.reconcileBackendTLSPolicy(ctx, ingress, service, policies.Items); err != nil {
			return err
		}
	}
	return nil
}

// warnBackendTLSSettings records a warning for each backend TLS setting of the Ingress that its
// BackendTLSPolicy cannot express.
func (r *IngressReconciler) warnBackendTLSSettings(ingress *networkingv1.Ingress) {
	if !translator.UsesBackendTLS(ingress) {
		return
	}
	_, warnings := translator.BackendTLSFor(ingress)
	for _, warning := range warnings {
		r.recordWarning(ingress, "BackendTLSSetting", warning)
	}
}

// reconcileBackendTLSPolicy makes the policy of one Service match the Ingresses using it.
func (r *IngressReconciler) reconcileBackendTLSPolicy(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	service string,
	policies []gatewayv1.BackendTLSPolicy,
) error {
	contributors, err := r.backendTLSContributors(ctx, ingress.Namespace, service)
	if err != nil {
		return err
	}
	name := backendTLSPolicyName(service)
	existing := findPolicy(policies, name)
	if existing != nil && !isManagedBackendTLSPolicy(existing) {
		existing = nil
	}

	if len(contributors) == 0 {
		if existing == nil {
			return nil
		}
		log.FromContext(ctx).Info("Deleting BackendTLSPolicy no Ingress needs", "namespace", ingress.Namespace, "name", name)
		if err := r.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete BackendTLSPolicy %s/%s: %w", ingress.Namespace, name, err)
		}
		return nil
	}

	if foreign := foreignPolicyForService(policies, service); foreign != "" {
		if isContributor(contributors, ingress) {
			r.recordWarning(ingress, "BackendTLSPolicyNotManaged", fmt.Sprintf(
				"Service %s already has BackendTLSPolicy %s, which ingress-doperator does not manage; leaving it in charge",
				service, foreign))
		}
		return nil
	}

	desired, err := r.desiredBackendTLSPolicy(ctx, ingress, service, contributors, existing)
	if err != nil {
		return err
	}
	return r.applyBackendTLSPolicy(ctx, existing, desired)
}

// backendTLSContributors returns the Ingresses in namespace, sorted by name, that ingress-doperator
// translates and that use the Service with backend-protocol: HTTPS.
func (r *IngressReconciler) backendTLSContributors(
	ctx context.Context,
	namespace, service string,
) ([]networkingv1.Ingress, error) {
	ingresses := &networkingv1.IngressList{}
	if err := r.List(ctx, ingresses, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("failed to list Ingresses in %s: %w", namespace, err)
	}
	var contributors []networkingv1.Ingress
	for _, candidate := range ingresses.Items {
		if !candidate.DeletionTimestamp.IsZero() || !translator.UsesBackendTLS(&candidate) ||
			candidate.Annotations[IgnoreIngressAnnotation] == annotationTrue || !r.shouldEnqueueIngress(&candidate) ||
			!slices.Contains(translator.BackendServiceNames(&candidate), service) {
			continue
		}
		contributors = append(contributors, candidate)
	}
	sort.Slice(contributors, func(i, j int) bool { return contributors[i].Name < contributors[j].Name })
	return contributors, nil
}

// desiredBackendTLSPolicy builds the policy for the Service: validation from the existing policy if
// a contributor still asks for it, else from the first contributor (so it does not flip between
// conflicting Ingresses), and target ports from all of them. Contributors asking for different
// validation get a warning when they are the reconciled Ingress.
func (r *IngressReconciler) desiredBackendTLSPolicy(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	service string,
	contributors []networkingv1.Ingress,
	existing *gatewayv1.BackendTLSPolicy,
) (*gatewayv1.BackendTLSPolicy, error) {
	validations := make([]gatewayv1.BackendTLSPolicyValidation, len(contributors))
	chosen, kept := 0, false
	for i := range contributors {
		settings, _ := translator.BackendTLSFor(&contributors[i])
		validations[i] = backendTLSValidation(settings, service, contributors[i].Namespace)
		if !kept && existing != nil && equality.Semantic.DeepEqual(existing.Spec.Validation, validations[i]) {
			chosen, kept = i, true
		}
	}
	for i := range contributors {
		if contributors[i].Name == ingress.Name && !equality.Semantic.DeepEqual(validations[i], validations[chosen]) {
			r.recordWarning(ingress, "BackendTLSPolicyConflict", fmt.Sprintf(
				"Ingress %s configures different backend TLS for Service %s; BackendTLSPolicy %s keeps the settings of %s",
				ingress.Name, service, backendTLSPolicyName(service), contributors[chosen].Name))
		}
	}

	targets, err := r.backendTLSTargets(ctx, service, contributors)
	if err != nil {
		return nil, err
	}
	sources := make([]string, 0, len(contributors))
	for _, contributor := range contributors {
		sources = append(sources, contributor.Namespace+"/"+contributor.Name)
	}
	return &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backendTLSPolicyName(service),
			Namespace: ingress.Namespace,
			Annotations: map[string]string{
				translator.ManagedByAnnotation: translator.ManagedByValue,
				translator.SourceAnnotation:    strings.Join(sources, ","),
			},
		},
		Spec: gatewayv1.BackendTLSPolicySpec{TargetRefs: targets, Validation: validations[chosen]},
	}, nil
}

// backendTLSValidation turns the backend TLS settings into the policy's validation; the hostname
// defaults to the Service's cluster DNS name.
func backendTLSValidation(settings translator.BackendTLS, service, namespace string) gatewayv1.BackendTLSPolicyValidation {
	hostname := settings.Hostname
	if hostname == "" {
		hostname = service + "." + namespace + ".svc"
	}
	validation := gatewayv1.BackendTLSPolicyValidation{Hostname: gatewayv1.PreciseHostname(hostname)}
	if settings.CAKind == "" {
		system := gatewayv1.WellKnownCACertificatesSystem
		validation.WellKnownCACertificates = &system
		return validation
	}
	validation.CACertificateRefs = []gatewayv1.LocalObjectReference{{
		Group: "", Kind: gatewayv1.Kind(settings.CAKind), Name: gatewayv1.ObjectName(settings.CAName),
	}}
	return validation
}

// backendTLSTargets targets the Service ports the contributors use, by port name. A port without a
// name (or a Service that does not exist yet) makes the policy target the whole Service.
func (r *IngressReconciler) backendTLSTargets(
	ctx context.Context,
	service string,
	contributors []networkingv1.Ingress,
) ([]gatewayv1.LocalPolicyTargetReferenceWithSectionName, error) {
	whole := []gatewayv1.LocalPolicyTargetReferenceWithSectionName{{
		LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{
			Group: "", Kind: serviceKind, Name: gatewayv1.ObjectName(service),
		},
	}}
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Namespace: contributors[0].Namespace, Name: service}, svc)
	if apierrors.IsNotFound(err) {
		return whole, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get Service %s/%s: %w", contributors[0].Namespace, service, err)
	}

	sections := map[string]bool{}
	for i := range contributors {
		for _, port := range servicePortsUsed(&contributors[i], service) {
			section := servicePortName(svc, port)
			if section == "" {
				return whole, nil
			}
			sections[section] = true
		}
	}
	targets := make([]gatewayv1.LocalPolicyTargetReferenceWithSectionName, 0, len(sections))
	for _, section := range sortedKeys(sections) {
		target := whole[0]
		sectionName := gatewayv1.SectionName(section)
		target.SectionName = &sectionName
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return whole, nil
	}
	return targets, nil
}

// servicePortsUsed returns the backend ports the Ingress uses for the Service.
func servicePortsUsed(ingress *networkingv1.Ingress, service string) []networkingv1.ServiceBackendPort {
	var ports []networkingv1.ServiceBackendPort
	add := func(backend *networkingv1.IngressBackend) {
		if backend != nil && backend.Service != nil && backend.Service.Name == service {
			ports = append(ports, backend.Service.Port)
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
	return ports
}

// servicePortName returns the name of the Service port the backend port refers to, or "" if the
// port has no name or does not exist.
func servicePortName(svc *corev1.Service, port networkingv1.ServiceBackendPort) string {
	for _, servicePort := range svc.Spec.Ports {
		if (port.Name != "" && servicePort.Name == port.Name) || (port.Name == "" && servicePort.Port == port.Number) {
			return servicePort.Name
		}
	}
	return ""
}

func (r *IngressReconciler) applyBackendTLSPolicy(
	ctx context.Context,
	existing, desired *gatewayv1.BackendTLSPolicy,
) error {
	logger := log.FromContext(ctx)
	if existing == nil {
		logger.Info("Creating BackendTLSPolicy", "namespace", desired.Namespace, "name", desired.Name)
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("failed to create BackendTLSPolicy %s/%s: %w", desired.Namespace, desired.Name, err)
		}
		return nil
	}
	if equality.Semantic.DeepEqual(existing.Spec, desired.Spec) &&
		existing.Annotations[translator.SourceAnnotation] == desired.Annotations[translator.SourceAnnotation] {
		return nil
	}
	updated := existing.DeepCopy()
	updated.Spec = desired.Spec
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	for key, value := range desired.Annotations {
		updated.Annotations[key] = value
	}
	logger.Info("Updating BackendTLSPolicy", "namespace", updated.Namespace, "name", updated.Name)
	if err := r.Update(ctx, updated); err != nil {
		return fmt.Errorf("failed to update BackendTLSPolicy %s/%s: %w", updated.Namespace, updated.Name, err)
	}
	return nil
}

func isManagedBackendTLSPolicy(policy *gatewayv1.BackendTLSPolicy) bool {
	return policy.Annotations[translator.ManagedByAnnotation] == translator.ManagedByValue
}

func policySources(policy *gatewayv1.BackendTLSPolicy) []string {
	raw := policy.Annotations[translator.SourceAnnotation]
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

// managedPolicyService returns the Service a policy managed by ingress-doperator targets.
func managedPolicyService(policy *gatewayv1.BackendTLSPolicy) string {
	for _, target := range policy.Spec.TargetRefs {
		if target.Group == "" && target.Kind == serviceKind {
			return string(target.Name)
		}
	}
	return ""
}

// foreignPolicyForService returns the name of a BackendTLSPolicy not managed by ingress-doperator
// that targets the Service (any of its ports), or "".
func foreignPolicyForService(policies []gatewayv1.BackendTLSPolicy, service string) string {
	for i := range policies {
		if isManagedBackendTLSPolicy(&policies[i]) {
			continue
		}
		for _, target := range policies[i].Spec.TargetRefs {
			if target.Group == "" && target.Kind == serviceKind && string(target.Name) == service {
				return policies[i].Name
			}
		}
	}
	return ""
}

func findPolicy(policies []gatewayv1.BackendTLSPolicy, name string) *gatewayv1.BackendTLSPolicy {
	for i := range policies {
		if policies[i].Name == name {
			return &policies[i]
		}
	}
	return nil
}

func isContributor(contributors []networkingv1.Ingress, ingress *networkingv1.Ingress) bool {
	for i := range contributors {
		if contributors[i].Name == ingress.Name {
			return true
		}
	}
	return false
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
