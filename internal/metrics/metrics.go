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

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// GatewayResourcesTotal tracks the total number of Gateway resources created or updated
	GatewayResourcesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ingress_operator_gateway_resources_total",
			Help: "Total number of Gateway resources created or updated by the ingress operator",
		},
		[]string{"operation", "namespace", "name"},
	)

	// HTTPRouteResourcesTotal tracks the total number of HTTPRoute resources created or updated
	HTTPRouteResourcesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ingress_operator_httproute_resources_total",
			Help: "Total number of HTTPRoute resources created or updated by the ingress operator",
		},
		[]string{"operation", "namespace", "name"},
	)

	// ReferenceGrantResourcesTotal tracks the total number of ReferenceGrant resources created or updated
	ReferenceGrantResourcesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ingress_operator_referencegrant_resources_total",
			Help: "Total number of ReferenceGrant resources created or updated by the ingress operator",
		},
		[]string{"operation", "namespace", "name"},
	)

	// IngressReconcileSkipsTotal tracks the number of reconciles skipped due to cache/disabled/etc.
	IngressReconcileSkipsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ingress_operator_reconcile_skips_total",
			Help: "Total number of ingress reconciles skipped",
		},
		[]string{"reason", "namespace", "name"},
	)
)

func init() {
	// Register custom metrics with the global prometheus registry
	metrics.Registry.MustRegister(
		GatewayResourcesTotal,
		HTTPRouteResourcesTotal,
		ReferenceGrantResourcesTotal,
		IngressReconcileSkipsTotal,
	)
}
