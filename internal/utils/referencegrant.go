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

package utils

import (
	"context"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/fiksn/ingress-operator/internal/translator"
)

// EnsureReferenceGrants creates ReferenceGrants for the given Ingresses
func EnsureReferenceGrants(
	ctx context.Context,
	c client.Client,
	trans *translator.Translator,
	ingresses []networkingv1.Ingress,
	recordMetric func(operation, namespace, name string),
) error {
	logger := log.FromContext(ctx)

	// Get namespaces that need ReferenceGrants using translator
	namespacesWithTLS := trans.GetNamespacesWithTLS(ingresses)

	// Create ReferenceGrant in each namespace
	for _, namespace := range namespacesWithTLS {
		if err := ApplyReferenceGrant(ctx, c, trans, namespace, recordMetric); err != nil {
			logger.Error(err, "failed to apply ReferenceGrant", "namespace", namespace)
			return err
		}
	}

	return nil
}

// CleanupReferenceGrantIfNeeded deletes the ReferenceGrant if no Ingresses with TLS remain in the namespace
func CleanupReferenceGrantIfNeeded(
	ctx context.Context,
	c client.Client,
	namespace string,
) error {
	logger := log.FromContext(ctx)

	// List all Ingresses in this namespace
	var ingressList networkingv1.IngressList
	if err := c.List(ctx, &ingressList, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("failed to list Ingresses: %w", err)
	}

	// Check if any Ingress has TLS configuration
	for _, ingress := range ingressList.Items {
		if len(ingress.Spec.TLS) > 0 {
			// Still have Ingresses with TLS, keep the ReferenceGrant
			logger.V(3).Info("ReferenceGrant still needed", "namespace", namespace,
				"reason", "Ingress with TLS exists")
			return nil
		}
	}

	// No Ingresses with TLS found, delete the ReferenceGrant if it exists and is managed by us
	refGrant := &gatewayv1beta1.ReferenceGrant{}
	err := c.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      translator.ReferenceGrantName,
	}, refGrant)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// Already deleted, nothing to do
			return nil
		}
		return err
	}

	// Delete if managed by us
	if IsManagedByUs(refGrant) {
		logger.Info("Deleting ReferenceGrant (no Ingresses with TLS remain)",
			"namespace", namespace, "name", translator.ReferenceGrantName)
		if err := c.Delete(ctx, refGrant); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete ReferenceGrant: %w", err)
		}
	} else {
		logger.Info("ReferenceGrant exists but is not managed by us, skipping deletion",
			"namespace", namespace, "name", translator.ReferenceGrantName)
	}

	return nil
}

// ApplyReferenceGrant creates or updates a ReferenceGrant in the given namespace
func ApplyReferenceGrant(
	ctx context.Context,
	c client.Client,
	trans *translator.Translator,
	ingressNamespace string,
	recordMetric func(operation, namespace, name string),
) error {
	logger := log.FromContext(ctx)

	// Create ReferenceGrant using translator
	refGrant := trans.CreateReferenceGrant(ingressNamespace)

	// Check if ReferenceGrant exists
	existing := &gatewayv1beta1.ReferenceGrant{}
	err := c.Get(ctx, types.NamespacedName{
		Namespace: ingressNamespace,
		Name:      translator.ReferenceGrantName,
	}, existing)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// Create new ReferenceGrant
			logger.Info("Creating ReferenceGrant", "namespace", ingressNamespace, "name", translator.ReferenceGrantName)
			if err := c.Create(ctx, refGrant); err != nil {
				return fmt.Errorf("failed to create ReferenceGrant: %w", err)
			}
			// Record metric if callback provided
			if recordMetric != nil {
				recordMetric("create", ingressNamespace, translator.ReferenceGrantName)
			}
			return nil
		}
		return err
	}

	// Update existing ReferenceGrant if managed by us
	if IsManagedByUs(existing) {
		existing.Spec = refGrant.Spec
		logger.Info("Updating ReferenceGrant", "namespace", ingressNamespace, "name", translator.ReferenceGrantName)
		if err := c.Update(ctx, existing); err != nil {
			return fmt.Errorf("failed to update ReferenceGrant: %w", err)
		}
		// Record metric if callback provided
		if recordMetric != nil {
			recordMetric("update", ingressNamespace, translator.ReferenceGrantName)
		}
	} else {
		logger.Info("ReferenceGrant exists but is not managed by us, skipping",
			"namespace", ingressNamespace, "name", translator.ReferenceGrantName)
	}

	return nil
}
