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
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/fiksn/ingress-doperator/internal/translator"
)

// externalNameRetryInterval is how often an Ingress skipped for its ExternalName backends is
// checked again, e.g. for a Service that is no longer of type ExternalName.
const externalNameRetryInterval = 5 * time.Minute

// skipForExternalNameBackends reports whether the Ingress must not be translated because it routes
// to ExternalName Services and --external-name-services=allow is not set. NGINX Gateway Fabric only
// proxies to ExternalName Services with a DNS resolver in the Gateway's NginxProxy; without one, a
// single such route makes NGINX reject the configuration of the whole Gateway. A skipped Ingress
// gets a warning, and the HTTPRoutes generated for it earlier are deleted so they no longer break
// the Gateway.
func (r *IngressReconciler) skipForExternalNameBackends(ctx context.Context, ingress *networkingv1.Ingress) (bool, error) {
	if r.AllowExternalNameServices {
		return false, nil
	}
	services, err := r.externalNameBackends(ctx, ingress)
	if err != nil || len(services) == 0 {
		return false, err
	}
	log.FromContext(ctx).Info("Skipping Ingress with ExternalName backends", "namespace", ingress.Namespace,
		"name", ingress.Name, "services", services)
	r.recordWarning(ingress, "ExternalNameBackendUnsupported", fmt.Sprintf(
		"not translated: backend Service(s) %s are of type ExternalName, which NGINX Gateway Fabric only "+
			"supports with a DNS resolver (spec.dnsResolver) in the Gateway's NginxProxy. Configure one and run "+
			"ingress-doperator with --external-name-services=allow.", strings.Join(services, ", ")))
	if err := r.HTTPRouteManager.ApplyHTTPRoutes(ctx, ingress, nil, nil); err != nil {
		return true, fmt.Errorf("failed to delete HTTPRoutes of skipped Ingress: %w", err)
	}
	return true, nil
}

// externalNameBackends returns the sorted names of the ExternalName Services the Ingress and its
// canary siblings route to. Services that do not exist are skipped.
func (r *IngressReconciler) externalNameBackends(ctx context.Context, ingress *networkingv1.Ingress) ([]string, error) {
	sources := append([]*networkingv1.Ingress{ingress}, r.findCanarySiblings(ctx, ingress)...)
	found := map[string]bool{}
	for _, source := range sources {
		for _, name := range translator.BackendServiceNames(source) {
			service := &corev1.Service{}
			err := r.Get(ctx, types.NamespacedName{Namespace: source.Namespace, Name: name}, service)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("failed to get Service %s/%s: %w", source.Namespace, name, err)
			}
			if service.Spec.Type == corev1.ServiceTypeExternalName {
				found[name] = true
			}
		}
	}
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
