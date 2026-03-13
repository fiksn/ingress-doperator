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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/metrics"
	"github.com/fiksn/ingress-doperator/internal/translator"
	"github.com/fiksn/ingress-doperator/internal/utils"
)

const (
	IngressClassAnnotation                   = "kubernetes.io/ingress.class"
	IngressDisabledAnnotation                = "ingress-doperator.fiction.si/disabled"
	IngressRemovedAnnotation                 = "ingress-doperator.fiction.si/removed"
	IgnoreIngressAnnotation                  = "ingress-doperator.fiction.si/ignore-ingress"
	OriginalIngressClassAnnotation           = "ingress-doperator.fiction.si/original-ingress-class"
	OriginalIngressClassNameAnnotation       = "ingress-doperator.fiction.si/original-ingress-classname"
	ExternalDNSIngressHostnameSource         = "external-dns.alpha.kubernetes.io/ingress-hostname-source"
	ExternalDNSGatewayHostnameSource         = "external-dns.alpha.kubernetes.io/gateway-hostname-source"
	ExternalDNSHostnameAnnotation            = "external-dns.alpha.kubernetes.io/hostname"
	OriginalExternalDNSHostname              = "ingress-doperator.fiction.si/original-external-dns-hostname"
	OriginalExternalDNSIngressHostnameSource = "ingress-doperator.fiction.si/original-external-dns-ingress-hostname-source"
	OriginalExternalDNSGatewayHostnameSource = "ingress-doperator.fiction.si/original-external-dns-gateway-hostname-source"
	ExternalDNSHostnameSourceAnnotationOnly  = "annotation-only"
	FinalizerName                            = "ingress-doperator.fiction.si/finalizer"
	HTTPRouteSnippetsFilterAnnotation        = "ingress-doperator.fiction.si/httproute-snippets-filter"
	HTTPRouteAuthenticationAnnotation        = "ingress-doperator.fiction.si/httproute-authentication-filter"
	HTTPRouteRequestHeaderAnnotation         = "ingress-doperator.fiction.si/httproute-request-header-modifier-filter"
	DisabledIngressClassName                 = "ingress-doperator-disabled"
	DisabledIngressClassController           = "dummy.io/no-controller"
	IngressDisabledReasonNormal              = "normal"
	IngressDisabledReasonExternalDNS         = "external-dns"
	DefaultGatewayAnnotationFilters          = "ingress.kubernetes.io," +
		"nginx.ingress.kubernetes.io,kubectl.kubernetes.io,kubernetes.io/ingress.class," +
		"traefik.ingress.kubernetes.io,ingress-doperator.fiction.si," +
		"external-dns.alpha.kubernetes.io"
	DefaultHTTPRouteAnnotationFilters = "ingress.kubernetes.io," +
		"nginx.ingress.kubernetes.io,kubectl.kubernetes.io,kubernetes.io/ingress.class," +
		"traefik.ingress.kubernetes.io,ingress-doperator.fiction.si"
)

// ingressPostProcessing
type IngressPostProcessingMode string

const (
	// IngressPostProcessingModeNone leaves the source Ingress unchanged
	IngressPostProcessingModeNone IngressPostProcessingMode = "none"
	// IngressPostProcessingModeDisable removes the ingress class to disable processing
	IngressPostProcessingModeDisable IngressPostProcessingMode = "disable"
	// IngressPostProcessingModeRemove deletes the source Ingress resource
	IngressPostProcessingModeRemove IngressPostProcessingMode = "remove"
	// IngressPostProcessingModeDisableExternalDNS forces external-dns to only read annotations
	IngressPostProcessingModeDisableExternalDNS IngressPostProcessingMode = "disable-external-dns"
)

const requeueAfterError = 30 * time.Second
const selfDeletedIngressTTL = 10 * time.Minute

type IngressReconciler struct {
	client.Client
	Scheme                           *runtime.Scheme
	Recorder                         events.EventRecorder
	GatewayNamespace                 string
	GatewayName                      string
	GatewayClassName                 string
	WatchNamespace                   string
	OneGatewayPerIngress             bool
	EnableDeletion                   bool
	HostnameRewriteFrom              string
	HostnameRewriteTo                string
	IngressPostProcessingMode        IngressPostProcessingMode
	GatewayAnnotationFilters         []string
	HTTPRouteAnnotationFilters       []string
	DefaultGatewayAnnotations        map[string]string
	GatewayInfrastructureAnnotations map[string]string
	InfrastructureAnnotationsByClass []translator.IngressClassAnnotationsRule
	IngressClassFilters              []string
	IngressClassIgnoreFilters        []string
	IngressClassEmpty                string
	UseIngress2Gateway               bool
	Ingress2GatewayProvider          string
	Ingress2GatewayIngressClass      string
	HTTPRouteManager                 *utils.HTTPRouteManager
	IngressClassSnippetsFilters      []utils.IngressClassSnippetsFilter
	IngressNameSnippetsFilters       []utils.IngressClassSnippetsFilter
	IngressAnnotationSnippetsAdd     []utils.IngressAnnotationSnippetsRule
	IngressAnnotationSnippetsRemove  []utils.IngressAnnotationSnippetsRule
	ClearIngressStatusOnDisable      bool
	ReconcileCache                   map[string]utils.ReconcileCacheEntry
	ReconcileCacheNamespace          string
	ReconcileCacheBaseName           string
	ReconcileCacheShards             int
	ReconcileCachePersist            bool
	ReconcileCacheMaxEntries         int
	SelfDeletedIngresses             map[string]time.Time
	SelfDeletedIngressesMu           sync.Mutex
	reconcileCacheMu                 sync.Mutex
	errorLogMu                       sync.Mutex
	errorLogLast                     map[string]time.Time
}

// getTranslator creates a translator instance with the reconciler's configuration
func (r *IngressReconciler) getTranslator() *translator.Translator {
	gatewayClassName := r.GatewayClassName
	if gatewayClassName == "" {
		gatewayClassName = "nginx"
	}
	return translator.New(translator.Config{
		GatewayNamespace:                 r.GatewayNamespace,
		GatewayName:                      r.GatewayName,
		GatewayClassName:                 gatewayClassName,
		HostnameRewriteFrom:              r.HostnameRewriteFrom,
		HostnameRewriteTo:                r.HostnameRewriteTo,
		DefaultGatewayAnnotations:        r.DefaultGatewayAnnotations,
		GatewayInfrastructureAnnotations: r.GatewayInfrastructureAnnotations,
		InfrastructureAnnotationsByClass: r.InfrastructureAnnotationsByClass,
		GatewayAnnotationFilters:         r.GatewayAnnotationFilters,
		HTTPRouteAnnotationFilters:       r.HTTPRouteAnnotationFilters,
		UseIngress2Gateway:               r.UseIngress2Gateway,
		Ingress2GatewayProvider:          r.Ingress2GatewayProvider,
		Ingress2GatewayIngressClass:      r.Ingress2GatewayIngressClass,
	})
}

func (r *IngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling Ingress", "namespace", req.Namespace, "name", req.Name)

	// Get the specific Ingress that triggered this reconciliation
	var ingress networkingv1.Ingress
	if err := r.Get(ctx, req.NamespacedName, &ingress); err != nil {
		if apierrors.IsNotFound(err) {
			// Ingress was deleted - this is normal, no error
			logger.V(1).Info("Ingress not found, likely deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch Ingress")
		return ctrl.Result{RequeueAfter: requeueAfterError}, nil
	}

	if r.shouldSkipIngress(&ingress, logger) {
		return ctrl.Result{}, nil
	}

	// Handle deletion
	if !ingress.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &ingress)
	}

	// Add finalizer if deletion is enabled and not already present
	if r.EnableDeletion && !utils.ContainsString(ingress.Finalizers, FinalizerName) {
		ingress.Finalizers = append(ingress.Finalizers, FinalizerName)
		if err := r.Update(ctx, &ingress); err != nil {
			logger.Error(err, "failed to add finalizer")
			return ctrl.Result{RequeueAfter: requeueAfterError}, nil
		}
		logger.V(1).Info("Added finalizer to Ingress")
	}

	// Translate this Ingress to HTTPRoute (Gateway listeners are managed by HTTPRoute controller)
	result, err := r.reconcileIngressToHTTPRoute(ctx, &ingress)
	r.maybeRecordReconcile(&ingress, result, err)
	return result, err
}

func (r *IngressReconciler) shouldSkipIngress(
	ingress *networkingv1.Ingress,
	logger logr.Logger,
) bool {
	if ingress == nil {
		return true
	}

	if ingress.Annotations != nil && ingress.Annotations[IgnoreIngressAnnotation] == fmt.Sprintf("%t", true) {
		logger.Info("Ingress has ignore annotation, skipping reconciliation")
		return true
	}

	if r.matchesIngressClassIgnoreFilter(ingress) {
		ingressClass := r.getIngressClass(ingress)
		logger.V(1).Info("Ingress class matches ignore filter, skipping reconciliation",
			"ingressClass", ingressClass,
			"ignoreFilter", strings.Join(r.IngressClassIgnoreFilters, ","))
		metrics.IngressReconcileSkipsTotal.WithLabelValues("class-ignore", ingress.Namespace, ingress.Name).Inc()
		return true
	}

	if !r.matchesIngressClassFilter(ingress) {
		ingressClass := r.getIngressClass(ingress)
		logger.V(1).Info("Ingress class does not match filter, skipping reconciliation",
			"ingressClass", ingressClass,
			"filter", strings.Join(r.IngressClassFilters, ","))
		metrics.IngressReconcileSkipsTotal.WithLabelValues("class-filter", ingress.Namespace, ingress.Name).Inc()
		return true
	}

	if ingress.Annotations != nil && ingress.Annotations[IngressRemovedAnnotation] == fmt.Sprintf("%t", true) {
		logger.Info("Ingress marked for removal by ingress-doperator, skipping reconciliation",
			"namespace", ingress.Namespace,
			"name", ingress.Name)
		metrics.IngressReconcileSkipsTotal.WithLabelValues("removed", ingress.Namespace, ingress.Name).Inc()
		return true
	}

	if r.getIngressClass(ingress) == DisabledIngressClassName {
		logger.Info("Ingress uses disabled class, skipping reconciliation",
			"namespace", ingress.Namespace,
			"name", ingress.Name)
		metrics.IngressReconcileSkipsTotal.WithLabelValues("disabled-class", ingress.Namespace, ingress.Name).Inc()
		return true
	}

	if r.wasSelfDeleted(ingress) {
		logger.Info("Ingress was deleted by ingress-doperator, skipping reconciliation",
			"namespace", ingress.Namespace,
			"name", ingress.Name)
		metrics.IngressReconcileSkipsTotal.WithLabelValues("self-deleted", ingress.Namespace, ingress.Name).Inc()
		return true
	}

	if r.shouldSkipReconcile(ingress) {
		logger.V(3).Info("Ingress resourceVersion unchanged, skipping reconciliation",
			"namespace", ingress.Namespace,
			"name", ingress.Name,
			"resourceVersion", ingress.ResourceVersion)
		metrics.IngressReconcileSkipsTotal.WithLabelValues("cache", ingress.Namespace, ingress.Name).Inc()
		return true
	}

	return false
}

// reconcileIngressToHTTPRoute translates a single Ingress to HTTPRoute
// Gateway listeners are managed separately by the HTTPRoute controller
func (r *IngressReconciler) reconcileIngressToHTTPRoute(
	ctx context.Context,
	ingress *networkingv1.Ingress,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if this Ingress should be processed
	if !r.shouldIncludeIngressForSynthesis(ingress, logger) {
		logger.V(1).Info("Skipping Ingress synthesis", "namespace", ingress.Namespace, "name", ingress.Name)
		return ctrl.Result{}, nil
	}

	// Handle source Ingress post-processing mode
	effectiveMode := r.resolveIngressPostProcessingMode(ingress)

	// Get translator
	trans := r.getTranslator()

	// Determine Gateway name based on mode
	var gatewayName string
	if r.OneGatewayPerIngress {
		// One Gateway per Ingress mode - use ingress name
		gatewayName = ingress.Name
	} else {
		// Shared Gateway mode - use ingress class
		ingressClass := r.getIngressClass(ingress)
		gatewayName = r.getGatewayNameForClass(ingressClass)
	}

	// Override gateway name in translator config
	transConfig := trans.Config
	transConfig.GatewayName = gatewayName
	singleTrans := translator.New(transConfig)

	// Translate to HTTPRoute (we no longer create Gateway here)
	httpRoute := singleTrans.TranslateToHTTPRoute(ingress)
	setHTTPRouteOwnerReference(httpRoute, ingress)

	// Apply extension refs (snippets, auth, headers)
	r.applyHTTPRouteExtensionRefs(ctx, ingress, httpRoute)

	// Resolve any named ports before applying
	if err := r.HTTPRouteManager.ResolveNamedPorts(ctx, ingress, httpRoute); err != nil {
		logger.Error(err, "failed to resolve named ports")
		// Continue anyway with fallback ports
	}

	// Split HTTPRoute if it exceeds the Gateway API limit
	httpRoutes := r.HTTPRouteManager.SplitHTTPRouteIfNeeded(httpRoute)

	// Apply all HTTPRoute(s) with proper cleanup of obsolete split routes
	metricRecorder := func(operation, namespace, name string) {
		metrics.HTTPRouteResourcesTotal.WithLabelValues(operation, namespace, name).Inc()
	}
	if err := r.HTTPRouteManager.ApplyHTTPRoutesAtomic(ctx, ingress, httpRoutes, metricRecorder); err != nil {
		logger.Error(err, "failed to apply HTTPRoutes")
		r.logErrorRateLimited(err, "apply-httproutes", "failed to apply HTTPRoutes")
		return ctrl.Result{}, err
	}

	logger.V(1).Info("HTTPRoute applied successfully", "namespace", httpRoute.Namespace, "name", httpRoute.Name)

	// Ensure Gateway listeners are updated from this Ingress change before post-processing
	listenerReconciler := &HTTPRouteReconciler{
		Client:              r.Client,
		GatewayNamespace:    r.GatewayNamespace,
		GatewayClassName:    r.GatewayClassName,
		HostnameRewriteFrom: r.HostnameRewriteFrom,
		HostnameRewriteTo:   r.HostnameRewriteTo,
	}

	gateway, canManageGateway, gatewayExists, err := r.ensureGatewayForListenerUpdate(ctx, gatewayName)
	if err != nil {
		logger.Error(err, "failed to ensure Gateway for listener update")
		return ctrl.Result{}, err
	}
	if !canManageGateway {
		logger.Info("Skipping Gateway listener update - Gateway is not managed by us",
			"namespace", r.GatewayNamespace,
			"name", gatewayName)
		return ctrl.Result{}, nil
	}

	// Ensure ReferenceGrant exists before updating Gateway listeners (cross-namespace secrets)
	for _, route := range httpRoutes {
		if route.Namespace != r.GatewayNamespace {
			if err := listenerReconciler.ensureReferenceGrant(ctx, route); err != nil {
				logger.Error(err, "failed to ensure ReferenceGrant for HTTPRoute",
					"namespace", route.Namespace,
					"name", route.Name)
			}
		}
	}

	updated := false
	for _, route := range httpRoutes {
		routeUpdated := listenerReconciler.updateGatewayListeners(ctx, gateway, route, ingress)
		if routeUpdated {
			updated = true
		}
	}
	if updated {
		if gatewayExists {
			if err := r.Update(ctx, gateway); err != nil {
				logger.Error(err, "failed to update Gateway after listener changes")
				return ctrl.Result{}, err
			}
			metrics.GatewayResourcesTotal.WithLabelValues("update", gateway.Namespace, gateway.Name).Inc()
		} else if len(gateway.Spec.Listeners) > 0 {
			if err := r.Create(ctx, gateway); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return ctrl.Result{RequeueAfter: requeueAfterError}, nil
				}
				logger.Error(err, "failed to create Gateway after listener changes")
				return ctrl.Result{}, err
			}
			metrics.GatewayResourcesTotal.WithLabelValues("create", gateway.Namespace, gateway.Name).Inc()
		}
		logger.Info("Updated Gateway listeners from Ingress", "gateway", gatewayName)
	}

	// Handle post-processing based on mode
	switch effectiveMode {
	case IngressPostProcessingModeRemove:
		if err := r.removeIngress(ctx, ingress); err != nil {
			logger.Error(err, "failed to remove source Ingress")
			return ctrl.Result{}, err
		}
		logger.Info("Removed source Ingress", "namespace", ingress.Namespace, "name", ingress.Name)
	case IngressPostProcessingModeDisable:
		if err := r.disableIngress(ctx, ingress); err != nil {
			logger.Error(err, "failed to disable source Ingress")
			return ctrl.Result{}, err
		}
		logger.Info("Disabled source Ingress", "namespace", ingress.Namespace, "name", ingress.Name)
	case IngressPostProcessingModeDisableExternalDNS:
		if err := r.disableExternalDNS(ctx, ingress); err != nil {
			logger.Error(err, "failed to disable external-dns for source Ingress")
			return ctrl.Result{}, err
		}
		logger.Info("Disabled external-dns on source Ingress", "namespace", ingress.Namespace, "name", ingress.Name)
	case IngressPostProcessingModeNone:
		// Do nothing
	}

	r.recordNormal(ingress, "ReconcileSuccess", "Ingress reconciled to HTTPRoute successfully")
	return ctrl.Result{}, nil
}

func (r *IngressReconciler) ensureGatewayForListenerUpdate(
	ctx context.Context,
	gatewayName string,
) (*gatewayv1.Gateway, bool, bool, error) {
	gatewayNN := types.NamespacedName{
		Namespace: r.GatewayNamespace,
		Name:      gatewayName,
	}
	gateway := &gatewayv1.Gateway{}
	if err := r.Get(ctx, gatewayNN, gateway); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, false, false, err
		}
		// Return a new Gateway object without creating it yet
		initializer := &HTTPRouteReconciler{
			GatewayNamespace: r.GatewayNamespace,
			GatewayClassName: r.GatewayClassName,
		}
		gateway = initializer.createInitialGateway(gatewayName)
		return gateway, true, false, nil
	}

	canManage, err := utils.CanUpdateResource(ctx, r.Client, gateway, gatewayNN)
	if err != nil {
		return nil, false, true, err
	}
	if !canManage {
		return gateway, false, true, nil
	}
	return gateway, true, true, nil
}

func (r *IngressReconciler) shouldIncludeIngressForSynthesis(
	ingress *networkingv1.Ingress,
	logger logr.Logger,
) bool {
	if ingress == nil {
		return false
	}

	if ingress.Annotations != nil && ingress.Annotations[IgnoreIngressAnnotation] == fmt.Sprintf("%t", true) {
		logger.V(1).Info("Ingress has ignore annotation, skipping synthesis",
			"namespace", ingress.Namespace,
			"name", ingress.Name)
		return false
	}

	if r.matchesIngressClassIgnoreFilter(ingress) {
		ingressClass := r.getIngressClass(ingress)
		logger.V(1).Info("Ingress class matches ignore filter, skipping synthesis",
			"ingressClass", ingressClass,
			"ignoreFilter", strings.Join(r.IngressClassIgnoreFilters, ","))
		metrics.IngressReconcileSkipsTotal.WithLabelValues("class-ignore", ingress.Namespace, ingress.Name).Inc()
		return false
	}

	if !r.matchesIngressClassFilter(ingress) {
		ingressClass := r.getIngressClass(ingress)
		logger.V(1).Info("Ingress class does not match filter, skipping synthesis",
			"ingressClass", ingressClass,
			"filter", strings.Join(r.IngressClassFilters, ","))
		metrics.IngressReconcileSkipsTotal.WithLabelValues("class-filter", ingress.Namespace, ingress.Name).Inc()
		return false
	}

	if ingress.Annotations != nil && ingress.Annotations[IngressRemovedAnnotation] == fmt.Sprintf("%t", true) {
		logger.V(1).Info("Ingress marked for removal by ingress-doperator, skipping synthesis",
			"namespace", ingress.Namespace,
			"name", ingress.Name)
		metrics.IngressReconcileSkipsTotal.WithLabelValues("removed", ingress.Namespace, ingress.Name).Inc()
		return false
	}

	if r.getIngressClass(ingress) == DisabledIngressClassName {
		logger.V(1).Info("Ingress uses disabled class, skipping synthesis",
			"namespace", ingress.Namespace,
			"name", ingress.Name)
		metrics.IngressReconcileSkipsTotal.WithLabelValues("disabled-class", ingress.Namespace, ingress.Name).Inc()
		return false
	}

	if r.wasSelfDeleted(ingress) {
		logger.V(1).Info("Ingress was deleted by ingress-doperator, skipping synthesis",
			"namespace", ingress.Namespace,
			"name", ingress.Name)
		metrics.IngressReconcileSkipsTotal.WithLabelValues("self-deleted", ingress.Namespace, ingress.Name).Inc()
		return false
	}

	return true
}

func (r *IngressReconciler) resolveIngressPostProcessingMode(
	ingress *networkingv1.Ingress,
) IngressPostProcessingMode {
	if ingress == nil || ingress.Annotations == nil {
		return r.IngressPostProcessingMode
	}
	switch ingress.Annotations[IngressDisabledAnnotation] {
	case IngressDisabledReasonNormal:
		return IngressPostProcessingModeDisable
	case IngressDisabledReasonExternalDNS:
		return IngressPostProcessingModeDisableExternalDNS
	default:
		return r.IngressPostProcessingMode
	}
}
func (r *IngressReconciler) applyHTTPRouteExtensionRefs(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	httpRoute *gatewayv1.HTTPRoute,
) {
	logger := log.FromContext(ctx)
	if ingress == nil || httpRoute == nil {
		return
	}

	r.applyAnnotationExtensionRefs(
		ctx,
		ingress,
		httpRoute,
		HTTPRouteAuthenticationAnnotation,
		utils.AuthenticationFilterKind,
		"AuthenticationFilter",
	)
	r.applyAnnotationExtensionRefs(
		ctx,
		ingress,
		httpRoute,
		HTTPRouteRequestHeaderAnnotation,
		utils.RequestHeaderModifierFilterKind,
		"RequestHeaderModifierFilter",
	)
	r.applySnippetsFilters(ctx, ingress, httpRoute, logger)
}

func (r *IngressReconciler) applyAnnotationExtensionRefs(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	httpRoute *gatewayv1.HTTPRoute,
	annotation string,
	kind string,
	logName string,
) {
	logger := log.FromContext(ctx)
	names := utils.ParseCommaSeparatedAnnotation(ingress.Annotations, annotation)
	if len(names) == 0 {
		return
	}

	available := make([]string, 0, len(names))
	for _, name := range names {
		ok, err := utils.EnsureExtensionResource(
			ctx,
			r.Client,
			kind,
			name,
			r.GatewayNamespace,
			ingress.Namespace,
			ingress.Namespace,
			ingress.Name,
			nil,
		)
		if err != nil {
			logger.Error(err, "failed to apply extension resource copy",
				"kind", logName,
				"name", name,
				"namespace", ingress.Namespace)
			continue
		}
		if ok {
			available = append(available, name)
		}
	}

	if len(available) > 0 {
		utils.AddExtensionRefFilters(httpRoute, utils.NginxGatewayGroup, kind, available)
	}
}

func (r *IngressReconciler) applySnippetsFilters(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	httpRoute *gatewayv1.HTTPRoute,
	logger logr.Logger,
) {
	snippetsOrder := make([]string, 0)
	snippetsSet := make(map[string]struct{})
	addSnippet := func(name string) {
		if name == "" {
			return
		}
		if _, ok := snippetsSet[name]; ok {
			return
		}
		snippetsSet[name] = struct{}{}
		snippetsOrder = append(snippetsOrder, name)
	}

	ingressClass := r.getIngressClass(ingress)
	for _, mapping := range r.IngressClassSnippetsFilters {
		if mapping.Pattern == "" {
			continue
		}
		matched, err := filepath.Match(mapping.Pattern, ingressClass)
		if err != nil {
			logger.Error(err, "invalid ingress class pattern for snippets filter", "pattern", mapping.Pattern)
			continue
		}
		if matched {
			addSnippet(mapping.Name)
		}
	}
	for _, mapping := range r.IngressNameSnippetsFilters {
		if mapping.Pattern == "" {
			continue
		}
		matched, err := filepath.Match(mapping.Pattern, ingress.Name)
		if err != nil {
			logger.Error(err, "invalid ingress name pattern for snippets filter", "pattern", mapping.Pattern)
			continue
		}
		if matched {
			addSnippet(mapping.Name)
		}
	}
	for _, name := range utils.ParseCommaSeparatedAnnotation(ingress.Annotations, HTTPRouteSnippetsFilterAnnotation) {
		addSnippet(name)
	}
	for _, name := range utils.MatchIngressAnnotationSnippetsRules(
		ingress.Annotations,
		r.IngressAnnotationSnippetsAdd,
	) {
		addSnippet(name)
	}

	removeSet := make(map[string]struct{})
	for _, name := range utils.MatchIngressAnnotationSnippetsRules(
		ingress.Annotations,
		r.IngressAnnotationSnippetsRemove,
	) {
		removeSet[name] = struct{}{}
	}
	if len(removeSet) > 0 && len(snippetsOrder) > 0 {
		filtered := make([]string, 0, len(snippetsOrder))
		for _, name := range snippetsOrder {
			if _, ok := removeSet[name]; ok {
				delete(snippetsSet, name)
				continue
			}
			filtered = append(filtered, name)
		}
		snippetsOrder = filtered
	}

	for _, name := range snippetsOrder {
		ok, err := utils.EnsureSnippetsFilterCopyForHTTPRoute(
			ctx,
			r.Client,
			r.Scheme,
			r.GatewayNamespace,
			httpRoute.Namespace,
			ingress.Namespace,
			ingress.Name,
			name,
			httpRoute.Name,
		)
		if err != nil {
			logger.Error(err, "failed to apply SnippetsFilter copy", "name", name, "namespace", ingress.Namespace)
			r.recordWarning(ingress, "SnippetsFilterCopyFailed",
				fmt.Sprintf("failed to copy SnippetsFilter %s from %s", name, r.GatewayNamespace))
			continue
		}
		if ok {
			utils.AddExtensionRefFilterAfterExistingKind(httpRoute, utils.NginxGatewayGroup, utils.SnippetsFilterKind, name)
		}
	}

	if ingress.Annotations != nil {
		for _, fullKey := range utils.NginxIngressSnippetWarningAnnotations(ingress.Annotations) {
			logger.Info("Ignoring nginx ingress annotation; use ingress-doperator.fiction.si/httproute-snippets-filter instead",
				"annotation", fullKey,
				"namespace", ingress.Namespace,
				"name", ingress.Name)
		}
	}
	snippets, warnings, ok := utils.BuildNginxIngressSnippets(ingress.Annotations)
	if !ok {
		return
	}
	for _, warning := range warnings {
		logger.Info("nginx ingress annotation warning",
			"warning", warning,
			"namespace", ingress.Namespace,
			"name", ingress.Name)
	}
	filterName := utils.AutomaticSnippetsFilterName(ingress.Name)
	var owner client.Object = ingress
	if r.IngressPostProcessingMode == IngressPostProcessingModeRemove {
		owner = nil
	}
	ready, err := utils.EnsureSnippetsFilterForIngress(
		ctx,
		r.Client,
		r.Scheme,
		httpRoute,
		owner,
		ingress.Namespace,
		ingress.Name,
		filterName,
		snippets,
	)
	if err != nil {
		logger.Error(err, "failed to apply annotation SnippetsFilter", "name", filterName, "namespace", httpRoute.Namespace)
		r.recordWarning(ingress, "AnnotationSnippetsFilterFailed",
			fmt.Sprintf("failed to apply annotation SnippetsFilter %s", filterName))
		return
	}
	if ready {
		utils.AddExtensionRefFilterAfterExistingKind(httpRoute, utils.NginxGatewayGroup, utils.SnippetsFilterKind, filterName)
	}
}

func (r *IngressReconciler) handleDeletion(ctx context.Context, ingress *networkingv1.Ingress) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Handling Ingress deletion", "namespace", ingress.Namespace, "name", ingress.Name)

	if r.shouldSkipDeletionCleanup(ctx, ingress, logger) {
		return ctrl.Result{}, nil
	}

	if !r.EnableDeletion {
		logger.V(1).Info("Deletion disabled - HTTPRoute and Gateway will not be deleted")
		return r.finalizeDeletion(ctx, ingress)
	}

	if err := r.deleteManagedHTTPRoutes(ctx, ingress, logger); err != nil {
		return ctrl.Result{}, err
	}

	return r.finalizeDeletion(ctx, ingress)
}

func (r *IngressReconciler) shouldSkipDeletionCleanup(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	logger logr.Logger,
) bool {
	if ingress.Annotations != nil && ingress.Annotations[IngressRemovedAnnotation] == fmt.Sprintf("%t", true) {
		logger.Info("Ingress deleted by ingress-doperator, skipping derived resource cleanup",
			"namespace", ingress.Namespace,
			"name", ingress.Name)
		_, _ = r.finalizeDeletion(ctx, ingress)
		return true
	}

	if r.wasSelfDeleted(ingress) {
		logger.Info("Ingress deleted by ingress-doperator (cached), skipping derived resource cleanup",
			"namespace", ingress.Namespace,
			"name", ingress.Name)
		_, _ = r.finalizeDeletion(ctx, ingress)
		return true
	}

	return false
}

func (r *IngressReconciler) deleteManagedHTTPRoutes(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	logger logr.Logger,
) error {
	routes, err := r.HTTPRouteManager.GetHTTPRoutesWithPrefix(ctx, ingress.Namespace, ingress.Name)
	if err != nil {
		return err
	}

	for _, route := range routes {
		httpRoute := &route
		if utils.IsManagedByUsForIngress(httpRoute, ingress.Namespace, ingress.Name) {
			logger.V(1).Info("Deleting managed HTTPRoute", "namespace", httpRoute.Namespace, "name", httpRoute.Name)
			if err := r.Delete(ctx, httpRoute); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "failed to delete HTTPRoute")
				return err
			}
			metrics.HTTPRouteResourcesTotal.WithLabelValues("delete", httpRoute.Namespace, httpRoute.Name).Inc()
		} else {
			logger.V(1).Info("HTTPRoute exists but is not managed by us for this Ingress, skipping deletion",
				"namespace", httpRoute.Namespace, "name", httpRoute.Name)
		}
	}
	return nil
}

func (r *IngressReconciler) finalizeDeletion(
	ctx context.Context,
	ingress *networkingv1.Ingress,
) (ctrl.Result, error) {
	if removeErr := r.removeFinalizer(ctx, ingress); removeErr != nil {
		log.FromContext(ctx).Error(removeErr, "failed to remove finalizer")
		return ctrl.Result{}, removeErr
	}
	r.evictReconcileCache(ingress)
	return ctrl.Result{}, nil
}

func (r *IngressReconciler) evictReconcileCache(ingress *networkingv1.Ingress) {
	if ingress == nil || r.ReconcileCache == nil {
		return
	}
	if string(ingress.UID) == "" {
		return
	}
	key := utils.ReconcileCacheKey(string(ingress.UID))
	r.reconcileCacheMu.Lock()
	delete(r.ReconcileCache, key)
	cacheCopyEntries := make(map[string]utils.ReconcileCacheEntry, len(r.ReconcileCache))
	for k, v := range r.ReconcileCache {
		cacheCopyEntries[k] = v
	}
	r.reconcileCacheMu.Unlock()

	if !r.ReconcileCachePersist {
		return
	}
	if r.ReconcileCacheBaseName == "" || r.ReconcileCacheNamespace == "" {
		return
	}
	if err := utils.SaveReconcileCacheSharded(
		context.Background(),
		r.Client,
		r.ReconcileCacheNamespace,
		r.ReconcileCacheBaseName,
		r.ReconcileCacheShards,
		cacheCopyEntries,
	); err != nil {
		log.FromContext(context.Background()).Error(err, "failed to persist reconcile cache eviction")
	}
}

func (r *IngressReconciler) evictOldestCacheEntries() {
	type entry struct {
		key string
		ts  int64
	}
	entries := make([]entry, 0, len(r.ReconcileCache))
	for key, value := range r.ReconcileCache {
		entries = append(entries, entry{key: key, ts: value.UpdatedAtUnix})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ts < entries[j].ts
	})
	for len(r.ReconcileCache) > r.ReconcileCacheMaxEntries && len(entries) > 0 {
		oldest := entries[0]
		delete(r.ReconcileCache, oldest.key)
		entries = entries[1:]
	}
}

func (r *IngressReconciler) markSelfDeleted(ingress *networkingv1.Ingress) {
	if ingress == nil || ingress.UID == "" {
		return
	}
	r.SelfDeletedIngressesMu.Lock()
	defer r.SelfDeletedIngressesMu.Unlock()
	if r.SelfDeletedIngresses == nil {
		r.SelfDeletedIngresses = make(map[string]time.Time)
	}
	r.SelfDeletedIngresses[string(ingress.UID)] = time.Now()
}

func (r *IngressReconciler) wasSelfDeleted(ingress *networkingv1.Ingress) bool {
	if ingress == nil || ingress.UID == "" {
		return false
	}
	r.SelfDeletedIngressesMu.Lock()
	defer r.SelfDeletedIngressesMu.Unlock()
	if r.SelfDeletedIngresses == nil {
		return false
	}
	cutoff := time.Now().Add(-selfDeletedIngressTTL)
	for uid, ts := range r.SelfDeletedIngresses {
		if ts.Before(cutoff) {
			delete(r.SelfDeletedIngresses, uid)
		}
	}
	ts, ok := r.SelfDeletedIngresses[string(ingress.UID)]
	if !ok {
		return false
	}
	return ts.After(cutoff)
}

func (r *IngressReconciler) removeFinalizer(ctx context.Context, ingress *networkingv1.Ingress) error {
	if utils.ContainsString(ingress.Finalizers, FinalizerName) {
		ingress.Finalizers = utils.RemoveString(ingress.Finalizers, FinalizerName)
		if err := r.Update(ctx, ingress); err != nil {
			return err
		}
	}
	return nil
}

func (r *IngressReconciler) disableIngress(ctx context.Context, ingress *networkingv1.Ingress) error {
	logger := log.FromContext(ctx)

	// Check if already disabled
	if ingress.Annotations != nil && ingress.Annotations[IngressDisabledAnnotation] == IngressDisabledReasonNormal {
		if r.ClearIngressStatusOnDisable {
			if err := r.clearIngressStatus(ctx, ingress); err != nil {
				return err
			}
		}
		return nil // Already disabled
	}

	if err := r.ensureDisabledIngressClass(ctx); err != nil {
		return err
	}

	modified := false

	// Save original ingressClassName if it exists
	if ingress.Spec.IngressClassName != nil && *ingress.Spec.IngressClassName != fmt.Sprintf("%t", true) {
		if ingress.Annotations == nil {
			ingress.Annotations = make(map[string]string)
		}
		// Save the original value
		if _, exists := ingress.Annotations[OriginalIngressClassNameAnnotation]; !exists {
			ingress.Annotations[OriginalIngressClassNameAnnotation] = *ingress.Spec.IngressClassName
			logger.Info("Saved original ingressClassName", "value", *ingress.Spec.IngressClassName)
		}
	}

	if ingress.Annotations == nil {
		ingress.Annotations = make(map[string]string)
	}

	// Save original ingress.class annotation if it exists
	if class, exists := ingress.Annotations[IngressClassAnnotation]; exists && class != "" {
		if _, saved := ingress.Annotations[OriginalIngressClassAnnotation]; !saved {
			ingress.Annotations[OriginalIngressClassAnnotation] = class
			logger.Info("Saved original ingress.class annotation", "value", class)
		}
	}

	if ingress.Spec.IngressClassName == nil || *ingress.Spec.IngressClassName != DisabledIngressClassName {
		ingress.Spec.IngressClassName = ptr.To(DisabledIngressClassName)
		modified = true
		logger.Info("Set spec.ingressClassName to disable Ingress", "value", DisabledIngressClassName)
	}

	if ingress.Annotations[IngressClassAnnotation] != DisabledIngressClassName {
		ingress.Annotations[IngressClassAnnotation] = DisabledIngressClassName
		modified = true
		logger.Info("Set kubernetes.io/ingress.class annotation to disable Ingress",
			"value", DisabledIngressClassName)
	}

	// Mark as disabled
	if modified {
		ingress.Annotations[IngressDisabledAnnotation] = IngressDisabledReasonNormal

		if err := r.Update(ctx, ingress); err != nil {
			return fmt.Errorf("failed to update Ingress to disable it: %w", err)
		}
		logger.Info("Successfully disabled source Ingress", "namespace", ingress.Namespace, "name", ingress.Name)
	}

	if r.ClearIngressStatusOnDisable {
		if err := r.clearIngressStatus(ctx, ingress); err != nil {
			return err
		}
	}

	return nil
}

func (r *IngressReconciler) disableExternalDNS(ctx context.Context, ingress *networkingv1.Ingress) error {
	logger := log.FromContext(ctx)

	if ingress.Annotations == nil {
		ingress.Annotations = make(map[string]string)
	}

	modified := false

	if hostname, exists := ingress.Annotations[ExternalDNSHostnameAnnotation]; exists && hostname != "" {
		if _, saved := ingress.Annotations[OriginalExternalDNSHostname]; !saved {
			ingress.Annotations[OriginalExternalDNSHostname] = hostname
			modified = true
			logger.Info("Found external-dns hostname annotation; storing original value",
				"value", hostname)
			r.recordWarning(ingress, "ExternalDNSHostnameAnnotationPresent",
				fmt.Sprintf("external-dns hostname annotation present (%s); stored in %s",
					hostname, OriginalExternalDNSHostname))
		}
	}

	if source, exists := ingress.Annotations[ExternalDNSIngressHostnameSource]; exists {
		if _, saved := ingress.Annotations[OriginalExternalDNSIngressHostnameSource]; !saved {
			ingress.Annotations[OriginalExternalDNSIngressHostnameSource] = source
			modified = true
			logger.Info("Found external-dns ingress hostname source annotation; storing original value",
				"value", source)
		}
	}

	if ingress.Annotations[IngressDisabledAnnotation] != IngressDisabledReasonNormal &&
		ingress.Annotations[IngressDisabledAnnotation] != IngressDisabledReasonExternalDNS {
		ingress.Annotations[IngressDisabledAnnotation] = IngressDisabledReasonExternalDNS
		modified = true
	}

	if ingress.Annotations[ExternalDNSIngressHostnameSource] != ExternalDNSHostnameSourceAnnotationOnly {
		ingress.Annotations[ExternalDNSIngressHostnameSource] = ExternalDNSHostnameSourceAnnotationOnly
		modified = true
		logger.Info("Set external-dns ingress hostname source to annotation-only",
			"value", ExternalDNSHostnameSourceAnnotationOnly)
	}

	if !modified {
		return nil
	}

	if err := r.Update(ctx, ingress); err != nil {
		return fmt.Errorf("failed to update Ingress to disable external-dns: %w", err)
	}

	logger.Info("Successfully updated Ingress to disable external-dns processing",
		"namespace", ingress.Namespace, "name", ingress.Name)
	return nil
}

func (r *IngressReconciler) ensureDisabledIngressClass(ctx context.Context) error {
	ingressClass := &networkingv1.IngressClass{}
	err := r.Get(ctx, types.NamespacedName{Name: DisabledIngressClassName}, ingressClass)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	ingressClass = &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: DisabledIngressClassName,
		},
		Spec: networkingv1.IngressClassSpec{
			Controller: DisabledIngressClassController,
		},
	}
	if err := r.Create(ctx, ingressClass); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create disabled IngressClass: %w", err)
	}
	return nil
}

func (r *IngressReconciler) clearIngressStatus(ctx context.Context, ingress *networkingv1.Ingress) error {
	if ingress == nil {
		return nil
	}
	if len(ingress.Status.LoadBalancer.Ingress) == 0 {
		return nil
	}
	for i := 0; i < 2; i++ {
		updated := ingress.DeepCopy()
		updated.Status.LoadBalancer = networkingv1.IngressLoadBalancerStatus{}
		if err := r.Status().Update(ctx, updated); err != nil {
			if apierrors.IsConflict(err) {
				if err := r.Get(ctx, types.NamespacedName{
					Namespace: ingress.Namespace,
					Name:      ingress.Name,
				}, ingress); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("failed to clear Ingress status.loadBalancer: %w", err)
		}
		return nil
	}
	return nil
}

func (r *IngressReconciler) removeIngress(ctx context.Context, ingress *networkingv1.Ingress) error {
	logger := log.FromContext(ctx)

	// Check if already marked for removal to avoid re-deletion
	if ingress.Annotations != nil && ingress.Annotations[IngressRemovedAnnotation] == fmt.Sprintf("%t", true) {
		logger.Info("Ingress already marked for removal, proceeding with deletion")
		r.markSelfDeleted(ingress)
		// Delete the Ingress resource without cascading to dependents
		orphan := metav1.DeletePropagationOrphan
		if err := r.Delete(ctx, ingress, &client.DeleteOptions{PropagationPolicy: &orphan}); err != nil {
			if apierrors.IsNotFound(err) {
				return nil // Already deleted
			}
			return fmt.Errorf("failed to delete source Ingress: %w", err)
		}
		logger.Info("Successfully deleted source Ingress", "namespace", ingress.Namespace, "name", ingress.Name)
		return nil
	}

	// First, mark the Ingress with removal annotation
	if ingress.Annotations == nil {
		ingress.Annotations = make(map[string]string)
	}
	ingress.Annotations[IngressRemovedAnnotation] = fmt.Sprintf("%t", true)

	// Also add ignore annotation to prevent re-reconciliation
	ingress.Annotations[IgnoreIngressAnnotation] = fmt.Sprintf("%t", true)

	if err := r.Update(ctx, ingress); err != nil {
		return fmt.Errorf("failed to mark Ingress for removal: %w", err)
	}
	logger.Info("Marked source Ingress for removal", "namespace", ingress.Namespace, "name", ingress.Name)

	r.markSelfDeleted(ingress)

	// Delete the Ingress resource without cascading to dependents
	orphan := metav1.DeletePropagationOrphan
	if err := r.Delete(ctx, ingress, &client.DeleteOptions{PropagationPolicy: &orphan}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // Already deleted
		}
		return fmt.Errorf("failed to delete source Ingress: %w", err)
	}
	logger.Info("Successfully deleted source Ingress", "namespace", ingress.Namespace, "name", ingress.Name)

	return nil
}

func (r *IngressReconciler) getGatewayNameForClass(ingressClass string) string {
	if ingressClass == "" || ingressClass == "default" {
		return r.GatewayName
	}
	return ingressClass
}

// getIngressClass returns the ingress class from spec.ingressClassName or the legacy annotation
func (r *IngressReconciler) getIngressClass(ingress *networkingv1.Ingress) string {
	// First check spec.ingressClassName
	if ingress.Spec.IngressClassName != nil && *ingress.Spec.IngressClassName != "" {
		return *ingress.Spec.IngressClassName
	}

	// Fallback to legacy annotation
	if ingress.Annotations != nil {
		if class, ok := ingress.Annotations[IngressClassAnnotation]; ok && class != "" {
			return class
		}
	}

	if r.IngressClassEmpty != "" {
		return r.IngressClassEmpty
	}
	return ""
}

// matchesIngressClassFilter checks if the Ingress class matches any configured filter pattern
func (r *IngressReconciler) matchesIngressClassFilter(ingress *networkingv1.Ingress) bool {
	return matchIngressClassPatterns(r.IngressClassFilters, r.getIngressClass(ingress), "ingress class filter")
}

func (r *IngressReconciler) matchesIngressClassIgnoreFilter(ingress *networkingv1.Ingress) bool {
	return matchIngressClassPatterns(r.IngressClassIgnoreFilters, r.getIngressClass(ingress), "ingress class ignore filter")
}

func matchIngressClassPatterns(patterns []string, ingressClass, label string) bool {
	if len(patterns) == 0 {
		return false
	}
	for _, pattern := range patterns {
		if pattern == "*" {
			return true
		}
	}
	if ingressClass == "" {
		return false
	}
	for _, pattern := range patterns {
		if pattern == "" || pattern == "*" {
			continue
		}
		matched, err := filepath.Match(pattern, ingressClass)
		if err != nil {
			log.Log.Error(err, "Invalid ingress class filter pattern", "pattern", pattern, "filterType", label)
			continue
		}
		if matched {
			return true
		}
	}
	return false
}

func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1.Ingress{})

	// If watching specific namespace, add namespace filter
	if r.WatchNamespace != "" {
		b = b.WithEventFilter(NamespaceFilter(r.WatchNamespace))
	}

	apiReader := mgr.GetAPIReader()
	ctx := context.Background()
	if version, ok, err := utils.GetCRDVersion(ctx, apiReader, utils.SnippetsFilterCRDName); err == nil && ok {
		snippetsGVK := schema.GroupVersionKind{
			Group:   utils.NginxGatewayGroup,
			Version: version,
			Kind:    utils.SnippetsFilterKind,
		}
		snippets := &unstructured.Unstructured{}
		snippets.SetGroupVersionKind(snippetsGVK)
		b = b.Watches(
			snippets,
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				return r.enqueueIngressesForSnippetsFilter(ctx, obj)
			}),
			ctrlbuilder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetNamespace() == r.GatewayNamespace
			})),
		)
	} else {
		log.FromContext(ctx).V(1).Info("SnippetsFilter CRD not installed, skipping watch")
	}

	if version, ok, err := utils.GetCRDVersion(ctx, apiReader, utils.AuthenticationFilterCRDName); err == nil && ok {
		authGVK := schema.GroupVersionKind{
			Group:   utils.NginxGatewayGroup,
			Version: version,
			Kind:    utils.AuthenticationFilterKind,
		}
		auth := &unstructured.Unstructured{}
		auth.SetGroupVersionKind(authGVK)
		b = b.Watches(
			auth,
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				return r.enqueueIngressesForExtension(ctx, obj, HTTPRouteAuthenticationAnnotation)
			}),
			ctrlbuilder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetNamespace() == r.GatewayNamespace
			})),
		)
	} else {
		log.FromContext(ctx).V(1).Info("AuthenticationFilter CRD not installed, skipping watch")
	}

	if version, ok, err := utils.GetCRDVersion(ctx, apiReader, utils.RequestHeaderModifierCRDName); err == nil && ok {
		headerGVK := schema.GroupVersionKind{
			Group:   utils.NginxGatewayGroup,
			Version: version,
			Kind:    utils.RequestHeaderModifierFilterKind,
		}
		header := &unstructured.Unstructured{}
		header.SetGroupVersionKind(headerGVK)
		b = b.Watches(
			header,
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				return r.enqueueIngressesForExtension(ctx, obj, HTTPRouteRequestHeaderAnnotation)
			}),
			ctrlbuilder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetNamespace() == r.GatewayNamespace
			})),
		)
	} else {
		log.FromContext(ctx).V(1).Info("RequestHeaderModifierFilter CRD not installed, skipping watch")
	}

	return b.Complete(r)
}

func (r *IngressReconciler) enqueueAllIngresses(ctx context.Context) []reconcile.Request {
	list := &networkingv1.IngressList{}
	opts := []client.ListOption{}
	if r.WatchNamespace != "" {
		opts = append(opts, client.InNamespace(r.WatchNamespace))
	}
	if err := r.List(ctx, list, opts...); err != nil {
		log.FromContext(ctx).Error(err, "failed to list Ingresses for extension filter change")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for _, ingress := range list.Items {
		if !r.shouldEnqueueIngressByClass(&ingress) {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: ingress.Namespace,
				Name:      ingress.Name,
			},
		})
	}
	return requests
}

func (r *IngressReconciler) enqueueIngressesForSnippetsFilter(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	if obj == nil {
		return r.enqueueAllIngresses(ctx)
	}
	filterName := obj.GetName()
	if filterName == "" {
		return nil
	}
	return r.enqueueIngressesForSnippetsName(ctx, filterName)
}

func (r *IngressReconciler) enqueueIngressesForExtension(
	ctx context.Context,
	obj client.Object,
	annotationKey string,
) []reconcile.Request {
	if obj == nil {
		return r.enqueueAllIngresses(ctx)
	}
	filterName := obj.GetName()
	if filterName == "" {
		return nil
	}
	return r.enqueueIngressesForAnnotation(ctx, filterName, annotationKey)
}

func (r *IngressReconciler) enqueueIngressesForSnippetsName(
	ctx context.Context,
	filterName string,
) []reconcile.Request {
	requests := r.enqueueIngressesForAnnotation(ctx, filterName, HTTPRouteSnippetsFilterAnnotation)
	list := &networkingv1.IngressList{}
	opts := []client.ListOption{}
	if r.WatchNamespace != "" {
		opts = append(opts, client.InNamespace(r.WatchNamespace))
	}
	if err := r.List(ctx, list, opts...); err != nil {
		log.FromContext(ctx).Error(err, "failed to list Ingresses for SnippetsFilter change")
		return requests
	}
	for _, ingress := range list.Items {
		if !r.shouldEnqueueIngressByClass(&ingress) {
			continue
		}
		ingressClass := r.getIngressClass(&ingress)
		for _, mapping := range r.IngressClassSnippetsFilters {
			if mapping.Pattern == "" || mapping.Name != filterName {
				continue
			}
			matched, err := filepath.Match(mapping.Pattern, ingressClass)
			if err != nil {
				continue
			}
			if !matched {
				continue
			}
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: ingress.Namespace,
					Name:      ingress.Name,
				},
			})
			break
		}
		for _, mapping := range r.IngressNameSnippetsFilters {
			if mapping.Pattern == "" || mapping.Name != filterName {
				continue
			}
			matched, err := filepath.Match(mapping.Pattern, ingress.Name)
			if err != nil {
				continue
			}
			if !matched {
				continue
			}
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: ingress.Namespace,
					Name:      ingress.Name,
				},
			})
			break
		}
		for _, name := range utils.MatchIngressAnnotationSnippetsRules(
			ingress.Annotations,
			r.IngressAnnotationSnippetsAdd,
		) {
			if name != filterName {
				continue
			}
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: ingress.Namespace,
					Name:      ingress.Name,
				},
			})
			break
		}
	}
	return dedupeReconcileRequests(requests)
}

func (r *IngressReconciler) enqueueIngressesForAnnotation(
	ctx context.Context,
	filterName string,
	annotationKey string,
) []reconcile.Request {
	list := &networkingv1.IngressList{}
	opts := []client.ListOption{}
	if r.WatchNamespace != "" {
		opts = append(opts, client.InNamespace(r.WatchNamespace))
	}
	if err := r.List(ctx, list, opts...); err != nil {
		log.FromContext(ctx).Error(err, "failed to list Ingresses for extension filter change")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for _, ingress := range list.Items {
		if !r.shouldEnqueueIngressByClass(&ingress) {
			continue
		}
		names := utils.ParseCommaSeparatedAnnotation(ingress.Annotations, annotationKey)
		for _, name := range names {
			if name == filterName {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: ingress.Namespace,
						Name:      ingress.Name,
					},
				})
				break
			}
		}
	}
	return requests
}

func dedupeReconcileRequests(requests []reconcile.Request) []reconcile.Request {
	if len(requests) <= 1 {
		return requests
	}
	seen := make(map[types.NamespacedName]struct{}, len(requests))
	out := make([]reconcile.Request, 0, len(requests))
	for _, req := range requests {
		if _, ok := seen[req.NamespacedName]; ok {
			continue
		}
		seen[req.NamespacedName] = struct{}{}
		out = append(out, req)
	}
	return out
}

func (r *IngressReconciler) shouldSkipReconcile(ingress *networkingv1.Ingress) bool {
	if ingress == nil {
		return false
	}
	if r.ReconcileCache == nil {
		return false
	}
	if ingress.ResourceVersion == "" {
		return false
	}
	key := utils.ReconcileCacheKey(string(ingress.UID))
	r.reconcileCacheMu.Lock()
	defer r.reconcileCacheMu.Unlock()
	last, ok := r.ReconcileCache[key]
	if !ok {
		return false
	}
	if last.ResourceVersion != ingress.ResourceVersion {
		return false
	}
	if time.Since(time.Unix(last.UpdatedAtUnix, 0)) > utils.ReconcileCacheTTL {
		delete(r.ReconcileCache, key)
		return false
	}
	return true
}

func (r *IngressReconciler) shouldEnqueueIngressByClass(ingress *networkingv1.Ingress) bool {
	if ingress == nil {
		return false
	}
	if r.matchesIngressClassIgnoreFilter(ingress) {
		return false
	}
	if !r.matchesIngressClassFilter(ingress) {
		return false
	}
	return true
}

func (r *IngressReconciler) maybeRecordReconcile(
	ingress *networkingv1.Ingress,
	result ctrl.Result,
	err error,
) {
	if ingress == nil {
		return
	}
	if err != nil || result.RequeueAfter != 0 {
		return
	}
	if r.ReconcileCache == nil {
		return
	}
	if ingress.ResourceVersion == "" {
		return
	}
	key := utils.ReconcileCacheKey(string(ingress.UID))
	r.reconcileCacheMu.Lock()
	r.ReconcileCache[key] = utils.ReconcileCacheEntry{
		ResourceVersion: ingress.ResourceVersion,
		UpdatedAtUnix:   time.Now().Unix(),
	}
	if r.ReconcileCacheMaxEntries > 0 && len(r.ReconcileCache) > r.ReconcileCacheMaxEntries {
		r.evictOldestCacheEntries()
	}
	cacheCopyEntries := make(map[string]utils.ReconcileCacheEntry, len(r.ReconcileCache))
	for k, v := range r.ReconcileCache {
		cacheCopyEntries[k] = v
	}
	r.reconcileCacheMu.Unlock()

	if !r.ReconcileCachePersist {
		return
	}
	if r.ReconcileCacheBaseName == "" || r.ReconcileCacheNamespace == "" {
		return
	}
	if err := utils.SaveReconcileCacheSharded(
		context.Background(),
		r.Client,
		r.ReconcileCacheNamespace,
		r.ReconcileCacheBaseName,
		r.ReconcileCacheShards,
		cacheCopyEntries,
	); err != nil {
		log.FromContext(context.Background()).Error(err, "failed to persist reconcile cache")
	}
}

func NamespaceFilter(namespace string) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == namespace
	})
}

func (r *IngressReconciler) recordWarning(ingress *networkingv1.Ingress, reason, message string) {
	if r.Recorder == nil || ingress == nil {
		return
	}
	r.Recorder.Eventf(ingress, nil, "Warning", reason, "Reconcile", message)
}

func (r *IngressReconciler) recordNormal(ingress *networkingv1.Ingress, reason, message string) {
	if r.Recorder == nil || ingress == nil {
		return
	}
	r.Recorder.Eventf(ingress, nil, "Normal", reason, "Reconcile", message)
}

func (r *IngressReconciler) logErrorRateLimited(err error, key, message string) {
	if err == nil {
		return
	}
	now := time.Now()
	r.errorLogMu.Lock()
	if r.errorLogLast == nil {
		r.errorLogLast = make(map[string]time.Time)
	}
	last, ok := r.errorLogLast[key]
	if ok && now.Sub(last) < 30*time.Second {
		r.errorLogMu.Unlock()
		return
	}
	r.errorLogLast[key] = now
	r.errorLogMu.Unlock()

	log.FromContext(context.Background()).Error(err, message)
}

func setHTTPRouteOwnerReference(httpRoute *gatewayv1.HTTPRoute, ingress *networkingv1.Ingress) {
	if httpRoute == nil || ingress == nil {
		return
	}
	controller := true
	blockOwnerDeletion := false
	httpRoute.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion:         ingress.APIVersion,
			Kind:               ingress.Kind,
			Name:               ingress.Name,
			UID:                ingress.UID,
			Controller:         &controller,
			BlockOwnerDeletion: &blockOwnerDeletion,
		},
	}
}
