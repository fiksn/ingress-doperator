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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	ManagedByAnnotation = "ingress-operator.fiction.si/managed-by"
	ManagedByValue      = "ingress-controller"
)

// IsManagedByUs checks if a resource is managed by the ingress operator
func IsManagedByUs(obj client.Object) bool {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return false
	}
	return annotations[ManagedByAnnotation] == ManagedByValue
}

// CanUpdateResource checks if we can create or update a resource
// Returns true if the resource doesn't exist or is managed by us
func CanUpdateResource(
	ctx context.Context,
	c client.Client,
	obj client.Object,
	namespacedName types.NamespacedName,
) (bool, error) {
	logger := log.FromContext(ctx)

	// Try to get the existing resource
	err := c.Get(ctx, namespacedName, obj)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Resource doesn't exist, we can create it
			return true, nil
		}
		// Some other error occurred
		return false, err
	}

	// Resource exists, check if it's managed by us
	if !IsManagedByUs(obj) {
		logger.Info("Resource exists but is not managed by us, skipping",
			"resource", obj.GetObjectKind().GroupVersionKind().Kind,
			"namespace", namespacedName.Namespace,
			"name", namespacedName.Name)
		return false, nil
	}

	// Resource exists and is managed by us, we can update it
	return true, nil
}
