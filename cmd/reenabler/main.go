/*
Copyright 2026.

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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"go.uber.org/zap/zapcore"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/controller"
	"github.com/fiksn/ingress-doperator/internal/utils"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

type trackedBool struct {
	value bool
	set   bool
}

func (b *trackedBool) Set(s string) error {
	b.set = true
	v, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	b.value = v
	return nil
}

func (b *trackedBool) String() string {
	return strconv.FormatBool(b.value)
}

func (b *trackedBool) IsBoolFlag() bool { return true }

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
}

func main() {
	var namespace string
	var verbosity int
	var removeDerivedResources bool
	var restore trackedBool
	var restoreClass trackedBool
	var restoreExternalDNS trackedBool
	var dangerouslyDeleteIngresses bool

	flag.StringVar(&namespace, "namespace", "", "If set, only process Ingresses in this namespace")
	flag.BoolVar(&removeDerivedResources, "remove-derived-resources", false,
		"If true, remove managed HTTPRoutes and automatic SnippetsFilters derived from the Ingress")
	restore.value = true
	flag.Var(&restore, "restore",
		"If true, restore both ingress class and external-dns annotations")
	flag.Var(&restoreClass, "restore-class",
		"If true, restore ingress class settings saved by ingress-doperator")
	flag.Var(&restoreExternalDNS, "restore-external-dns",
		"If true, restore external-dns annotations saved by ingress-doperator")
	flag.BoolVar(&dangerouslyDeleteIngresses, "dangerously-delete-ingresses", false,
		"If true, delete disabled Ingresses managed by ingress-doperator")
	flag.IntVar(&verbosity, "v", 0, "Log verbosity (0 = info, higher = more verbose)")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if verbosity > 0 {
		opts.Development = false
		opts.Level = zapcore.Level(-verbosity)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if dangerouslyDeleteIngresses {
		if restoreClass.set || restoreExternalDNS.set {
			setupLog.Error(fmt.Errorf("invalid flag combination"),
				"--dangerously-delete-ingresses cannot be combined with restore flags")
			os.Exit(1)
		}
		if removeDerivedResources {
			setupLog.Error(fmt.Errorf("invalid flag combination"),
				"--dangerously-delete-ingresses cannot be combined with --remove-derived-resources")
			os.Exit(1)
		}
		restore.value = false
	}

	if removeDerivedResources && (restoreClass.set || restoreExternalDNS.set || restore.value) {
		setupLog.Error(fmt.Errorf("invalid flag combination"),
			"--remove-derived-resources cannot be combined with restore flags")
		os.Exit(1)
	}

	if restoreClass.set || restoreExternalDNS.set {
		restore.value = false
	}

	if restore.value {
		restoreClass.value = true
		restoreExternalDNS.value = true
	}

	cfg := ctrl.GetConfigOrDie()
	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create Kubernetes client")
		os.Exit(1)
	}

	ctx := context.Background()
	if err := runReenabler(
		ctx,
		cli,
		namespace,
		removeDerivedResources,
		restoreClass.value,
		restoreExternalDNS.value,
		dangerouslyDeleteIngresses,
	); err != nil {
		setupLog.Error(err, "reenabler failed")
		os.Exit(1)
	}
}

func runReenabler(
	ctx context.Context,
	cli client.Client,
	namespace string,
	removeDerivedResources bool,
	restoreClass bool,
	restoreExternalDNS bool,
	dangerouslyDeleteIngresses bool,
) error {
	list := &networkingv1.IngressList{}
	if namespace != "" {
		if err := cli.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return err
		}
	} else {
		if err := cli.List(ctx, list); err != nil {
			return err
		}
	}

	manager := utils.HTTPRouteManager{Client: cli}

	if dangerouslyDeleteIngresses {
		for i := range list.Items {
			ingress := &list.Items[i]
			ok, reason, err := checkDeleteEligibility(ctx, cli, &manager, ingress)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("refusing to delete ingress %s/%s: %s", ingress.Namespace, ingress.Name, reason)
			}
		}
	}

	for i := range list.Items {
		ingress := &list.Items[i]
		disabled := isDisabledIngress(ingress)
		if dangerouslyDeleteIngresses && shouldDeleteIngress(ingress) {
			ok, reason, err := checkDeleteEligibility(ctx, cli, &manager, ingress)
			if err != nil {
				setupLog.Error(err, "failed to verify managed resources for ingress",
					"namespace", ingress.Namespace,
					"name", ingress.Name)
				continue
			}
			if !ok {
				setupLog.Info("Skipping ingress deletion due to failed checks",
					"namespace", ingress.Namespace,
					"name", ingress.Name,
					"reason", reason)
				continue
			}
			if err := cli.Delete(ctx, ingress); err != nil {
				setupLog.Error(err, "failed to delete ingress",
					"namespace", ingress.Namespace,
					"name", ingress.Name)
				continue
			}
			setupLog.Info("Deleted disabled Ingress",
				"namespace", ingress.Namespace,
				"name", ingress.Name)
			continue
		}
		if !disabled && (!restoreExternalDNS || !needsExternalDNSRestore(ingress)) {
			continue
		}

		if err := restoreIngressState(ctx, cli, ingress, disabled && restoreClass, restoreExternalDNS); err != nil {
			setupLog.Error(err, "failed to restore ingress state",
				"namespace", ingress.Namespace,
				"name", ingress.Name)
			continue
		}

		if disabled && restoreClass && removeDerivedResources {
			if err := removeManagedHTTPRoutes(ctx, &manager, ingress); err != nil {
				setupLog.Error(err, "failed to remove HTTPRoutes for ingress",
					"namespace", ingress.Namespace,
					"name", ingress.Name)
				continue
			}
			if err := removeAutomaticSnippetsFilter(ctx, cli, ingress); err != nil {
				setupLog.Error(err, "failed to remove automatic SnippetsFilter for ingress",
					"namespace", ingress.Namespace,
					"name", ingress.Name)
				continue
			}
		} else if disabled && restoreClass {
			setupLog.Info("Leaving derived resources in place (remove-derived-resources=false)",
				"namespace", ingress.Namespace,
				"name", ingress.Name)
		}

		if disabled && restoreClass {
			setupLog.Info("Re-enabled Ingress",
				"namespace", ingress.Namespace,
				"name", ingress.Name)
		} else {
			setupLog.Info("Restored external-dns annotations for Ingress",
				"namespace", ingress.Namespace,
				"name", ingress.Name)
		}
	}

	return nil
}

func isDisabledIngress(ingress *networkingv1.Ingress) bool {
	if ingress == nil {
		return false
	}
	if ingress.Annotations == nil {
		return false
	}
	if ingress.Annotations[controller.IngressDisabledAnnotation] == "" {
		return false
	}
	if ingress.Spec.IngressClassName != nil &&
		*ingress.Spec.IngressClassName == controller.DisabledIngressClassName {
		return true
	}
	if ingress.Annotations[controller.IngressClassAnnotation] == controller.DisabledIngressClassName {
		return true
	}
	return false
}

func restoreIngressState(
	ctx context.Context,
	cli client.Client,
	ingress *networkingv1.Ingress,
	restoreClass bool,
	restoreExternalDNS bool,
) error {
	if ingress == nil {
		return nil
	}
	updated := ingress.DeepCopy()
	annotations := updated.Annotations
	if annotations == nil {
		annotations = map[string]string{}
	}

	modified := false
	if restoreClass {
		originalClassName := annotations[controller.OriginalIngressClassNameAnnotation]
		originalClassAnnotation := annotations[controller.OriginalIngressClassAnnotation]

		if originalClassName != "" {
			updated.Spec.IngressClassName = &originalClassName
		} else {
			updated.Spec.IngressClassName = nil
		}

		if originalClassAnnotation != "" {
			annotations[controller.IngressClassAnnotation] = originalClassAnnotation
		} else {
			delete(annotations, controller.IngressClassAnnotation)
		}

		delete(annotations, controller.IngressDisabledAnnotation)
		delete(annotations, controller.OriginalIngressClassNameAnnotation)
		delete(annotations, controller.OriginalIngressClassAnnotation)
		modified = true
	}

	if restoreExternalDNS {
		if originalHostname, ok := annotations[controller.OriginalExternalDNSHostname]; ok {
			if originalHostname != "" {
				annotations[controller.ExternalDNSHostnameAnnotation] = originalHostname
			} else {
				delete(annotations, controller.ExternalDNSHostnameAnnotation)
			}
			delete(annotations, controller.OriginalExternalDNSHostname)
			modified = true
		}

		if originalSource, ok := annotations[controller.OriginalExternalDNSIngressHostnameSource]; ok {
			if originalSource != "" {
				annotations[controller.ExternalDNSIngressHostnameSource] = originalSource
			} else {
				delete(annotations, controller.ExternalDNSIngressHostnameSource)
			}
			delete(annotations, controller.OriginalExternalDNSIngressHostnameSource)
			modified = true
		} else if _, exists := annotations[controller.ExternalDNSIngressHostnameSource]; exists {
			delete(annotations, controller.ExternalDNSIngressHostnameSource)
			modified = true
		}
	}

	updated.Annotations = annotations
	if !modified {
		return nil
	}
	return cli.Update(ctx, updated)
}

func needsExternalDNSRestore(ingress *networkingv1.Ingress) bool {
	if ingress == nil || ingress.Annotations == nil {
		return false
	}
	if _, ok := ingress.Annotations[controller.OriginalExternalDNSHostname]; ok {
		return true
	}
	if _, ok := ingress.Annotations[controller.OriginalExternalDNSIngressHostnameSource]; ok {
		return true
	}
	_, ok := ingress.Annotations[controller.ExternalDNSIngressHostnameSource]
	return ok
}

func shouldDeleteIngress(ingress *networkingv1.Ingress) bool {
	if ingress == nil || ingress.Annotations == nil {
		return false
	}
	disabledValue := ingress.Annotations[controller.IngressDisabledAnnotation]
	if disabledValue == "" {
		return false
	}
	switch disabledValue {
	case controller.IngressDisabledReasonNormal:
		return isDisabledIngress(ingress)
	case controller.IngressDisabledReasonExternalDNS:
		if ingress.Annotations[controller.ExternalDNSIngressHostnameSource] == "" {
			return false
		}
		if _, ok := ingress.Annotations[controller.OriginalExternalDNSHostname]; ok {
			return true
		}
		if _, ok := ingress.Annotations[controller.OriginalExternalDNSIngressHostnameSource]; ok {
			return true
		}
		return false
	default:
		return false
	}
}

func checkDeleteEligibility(
	ctx context.Context,
	cli client.Client,
	manager *utils.HTTPRouteManager,
	ingress *networkingv1.Ingress,
) (bool, string, error) {
	if ingress == nil {
		return false, "missing ingress", nil
	}
	if ingress.Annotations == nil || ingress.Annotations[controller.IngressDisabledAnnotation] == "" {
		return false, "missing ingress-doperator disabled annotation", nil
	}
	if !shouldDeleteIngress(ingress) {
		return false, "disabled annotation does not satisfy delete criteria", nil
	}

	hasRoute, hasGateway, err := hasManagedResources(ctx, cli, manager, ingress)
	if err != nil {
		return false, "", err
	}
	if !hasRoute && !hasGateway {
		return false, "missing managed HTTPRoute and Gateway", nil
	}
	if !hasRoute {
		return false, "missing managed HTTPRoute", nil
	}
	if !hasGateway {
		return false, "missing managed Gateway", nil
	}
	return true, "", nil
}

func removeManagedHTTPRoutes(
	ctx context.Context,
	manager *utils.HTTPRouteManager,
	ingress *networkingv1.Ingress,
) error {
	if manager == nil || ingress == nil {
		return nil
	}
	routes, err := manager.GetHTTPRoutesWithPrefix(ctx, ingress.Namespace, ingress.Name)
	if err != nil {
		return err
	}

	for _, route := range routes {
		httpRoute := route
		if !utils.IsManagedByUsForIngress(&httpRoute, ingress.Namespace, ingress.Name) {
			continue
		}
		if err := manager.Client.Delete(ctx, &httpRoute); err != nil {
			return err
		}
	}

	return nil
}

func hasManagedResources(
	ctx context.Context,
	cli client.Client,
	manager *utils.HTTPRouteManager,
	ingress *networkingv1.Ingress,
) (bool, bool, error) {
	if manager == nil || ingress == nil {
		return false, false, nil
	}
	routes, err := manager.GetHTTPRoutesWithPrefix(ctx, ingress.Namespace, ingress.Name)
	if err != nil {
		return false, false, err
	}
	hasRoute := false
	for _, route := range routes {
		httpRoute := route
		if utils.IsManagedByUsForIngress(&httpRoute, ingress.Namespace, ingress.Name) {
			hasRoute = true
			break
		}
	}
	if !hasRoute {
		return false, false, nil
	}

	gateways := &gatewayv1.GatewayList{}
	if err := cli.List(ctx, gateways); err != nil {
		return false, false, err
	}
	for i := range gateways.Items {
		gateway := &gateways.Items[i]
		if utils.IsManagedByUsWithIngress(gateway, ingress.Namespace, ingress.Name) {
			return true, true, nil
		}
	}

	return hasRoute, false, nil
}

func removeAutomaticSnippetsFilter(ctx context.Context, cli client.Client, ingress *networkingv1.Ingress) error {
	if ingress == nil {
		return nil
	}
	filterName := utils.AutomaticSnippetsFilterName(ingress.Name)
	version, ok, err := utils.GetCRDVersion(ctx, cli, utils.SnippetsFilterCRDName)
	if err != nil || !ok {
		return err
	}
	filter := &unstructured.Unstructured{}
	filter.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   utils.NginxGatewayGroup,
		Version: version,
		Kind:    utils.SnippetsFilterKind,
	})
	if err := cli.Get(ctx, client.ObjectKey{Namespace: ingress.Namespace, Name: filterName}, filter); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !utils.IsManagedByUs(filter) {
		return nil
	}
	return cli.Delete(ctx, filter)
}
