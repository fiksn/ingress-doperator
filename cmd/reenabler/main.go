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

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
}

func main() {
	var namespace string
	var verbosity int
	var removeDerivedResources bool

	flag.StringVar(&namespace, "namespace", "", "If set, only process Ingresses in this namespace")
	flag.BoolVar(&removeDerivedResources, "remove-derived-resources", false,
		"If true, remove managed HTTPRoutes and automatic SnippetsFilters derived from the Ingress")
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

	cfg := ctrl.GetConfigOrDie()
	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create Kubernetes client")
		os.Exit(1)
	}

	ctx := context.Background()
	if err := runReenabler(ctx, cli, namespace, removeDerivedResources); err != nil {
		setupLog.Error(err, "reenabler failed")
		os.Exit(1)
	}
}

func runReenabler(ctx context.Context, cli client.Client, namespace string, removeDerivedResources bool) error {
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

	for i := range list.Items {
		ingress := &list.Items[i]
		if !isDisabledIngress(ingress) {
			continue
		}

		if err := restoreIngressClass(ctx, cli, ingress); err != nil {
			setupLog.Error(err, "failed to restore ingress class",
				"namespace", ingress.Namespace,
				"name", ingress.Name)
			continue
		}

		if removeDerivedResources {
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
		} else {
			setupLog.Info("Leaving derived resources in place (remove-derived-resources=false)",
				"namespace", ingress.Namespace,
				"name", ingress.Name)
		}

		setupLog.Info("Re-enabled Ingress",
			"namespace", ingress.Namespace,
			"name", ingress.Name)
	}

	return nil
}

func isDisabledIngress(ingress *networkingv1.Ingress) bool {
	if ingress == nil {
		return false
	}
	if ingress.Spec.IngressClassName != nil &&
		*ingress.Spec.IngressClassName == controller.DisabledIngressClassName {
		return true
	}
	if ingress.Annotations == nil {
		return false
	}
	if ingress.Annotations[controller.IngressClassAnnotation] == controller.DisabledIngressClassName {
		return true
	}
	return ingress.Annotations[controller.IngressDisabledAnnotation] == fmt.Sprintf("%t", true)
}

func restoreIngressClass(ctx context.Context, cli client.Client, ingress *networkingv1.Ingress) error {
	if ingress == nil {
		return nil
	}
	updated := ingress.DeepCopy()
	annotations := updated.Annotations
	if annotations == nil {
		annotations = map[string]string{}
	}

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

	updated.Annotations = annotations
	return cli.Update(ctx, updated)
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
