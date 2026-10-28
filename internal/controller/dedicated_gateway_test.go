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
	"errors"
	"reflect"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/translator"
	"github.com/fiksn/ingress-doperator/internal/utils"
)

func TestParseOneGatewayPerIngress(t *testing.T) {
	tests := []struct {
		value   string
		want    []string
		wantErr bool
	}{
		{value: "", want: nil},
		{value: "  ", want: nil},
		{value: "false", want: nil},
		{value: "true", want: []string{"*/*"}},
		{value: "TRUE", want: []string{"*/*"}},
		{value: "prod/api-*, */public", want: []string{"prod/api-*", "*/public"}},
		{value: "prod/a,prod/a,", want: []string{"prod/a"}},
		{value: "prod", wantErr: true},
		{value: "*", wantErr: true},
		{value: "a/b/c", wantErr: true},
		{value: "prod/[x", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseOneGatewayPerIngress(tt.value)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseOneGatewayPerIngress(%q) = %v, want error", tt.value, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseOneGatewayPerIngress(%q) unexpected error: %v", tt.value, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseOneGatewayPerIngress(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}
}

func TestGatewayNameFor(t *testing.T) {
	ingress := func(namespace, name, class string) *networkingv1.Ingress {
		ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
		if class != "" {
			ing.Spec.IngressClassName = ptr.To(class)
		}
		return ing
	}
	tests := []struct {
		name     string
		patterns []string
		ingress  *networkingv1.Ingress
		want     string
	}{
		{"no patterns uses class", nil, ingress("prod", "api", "nginx"), "nginx"},
		{"no patterns and no class uses shared name", nil, ingress("prod", "api", ""), "shared"},
		{"match all", []string{"*/*"}, ingress("prod", "api", "nginx"), dedicatedGatewayName("prod", "api")},
		{"name glob matches", []string{"prod/api-*"}, ingress("prod", "api-v2", "nginx"), dedicatedGatewayName("prod", "api-v2")},
		{"other namespace does not match", []string{"prod/api-*"}, ingress("dev", "api-v2", "nginx"), "nginx"},
		{"star does not cross slash", []string{"prod*"}, ingress("prod", "api", "nginx"), "nginx"},
		{"any pattern may match", []string{"x/y", "*/api"}, ingress("dev", "api", "nginx"), dedicatedGatewayName("dev", "api")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &IngressReconciler{GatewayName: "shared", OneGatewayPerIngress: tt.patterns}
			if got := r.gatewayNameFor(tt.ingress); got != tt.want {
				t.Errorf("gatewayNameFor() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDedicatedGatewayName(t *testing.T) {
	long := strings.Repeat("n", 253)
	tests := []struct {
		name           string
		namespace      string
		ingress        string
		readablePrefix string
		wantTruncation bool
	}{
		{"short", "prod", "api", "prod-api-", false},
		{"dots replaced", "prod", "api.v2", "prod-api-v2-", false},
		{"max lengths", strings.Repeat("s", 63), long, strings.Repeat("s", 54), true},
		{"truncation trims trailing dash", strings.Repeat("s", 53), long, strings.Repeat("s", 53) + "-", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dedicatedGatewayName(tt.namespace, tt.ingress)
			if errs := validation.IsDNS1123Label(got); len(errs) > 0 {
				t.Errorf("dedicatedGatewayName() = %q is not a DNS label: %v", got, errs)
			}
			if !strings.HasPrefix(got, tt.readablePrefix) {
				t.Errorf("dedicatedGatewayName() = %q, want prefix %q", got, tt.readablePrefix)
			}
			if strings.Contains(got, "--") {
				t.Errorf("dedicatedGatewayName() = %q contains a double dash", got)
			}
			if got != dedicatedGatewayName(tt.namespace, tt.ingress) {
				t.Error("dedicatedGatewayName() must be deterministic")
			}
		})
	}

	if dedicatedGatewayName("a-b", "c") == dedicatedGatewayName("a", "b-c") {
		t.Error("a-b/c and a/b-c must get different Gateway names")
	}
	if dedicatedGatewayName("ns", long+"x") == dedicatedGatewayName("ns", long+"y") {
		t.Error("names differing only past the truncation point must get different Gateway names")
	}
}

const testGatewayNamespace = "gateways"

func testGateway(name string, managed bool, hostnames ...string) *gatewayv1.Gateway {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testGatewayNamespace},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "nginx"},
	}
	if managed {
		gw.Annotations = map[string]string{translator.ManagedByAnnotation: translator.ManagedByValue}
	}
	for _, host := range hostnames {
		gw.Spec.Listeners = append(gw.Spec.Listeners, gatewayv1.Listener{
			Name:     gatewayv1.SectionName(host),
			Hostname: ptr.To(gatewayv1.Hostname(host)),
			Port:     443,
			Protocol: gatewayv1.HTTPSProtocolType,
		})
	}
	return gw
}

func testRoute(name, gateway, listenerGateway, hostname string) *gatewayv1.HTTPRoute {
	annotations := map[string]string{translator.ManagedByAnnotation: translator.ManagedByValue}
	if listenerGateway != "" {
		annotations[utils.ListenerGatewayAnnotation] = listenerGateway
	}
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "app", Annotations: annotations},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
				Name:      gatewayv1.ObjectName(gateway),
				Namespace: ptr.To(gatewayv1.Namespace(testGatewayNamespace)),
			}}},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(hostname)},
		},
	}
}

func newReleaseReconciler(t *testing.T, objs ...client.Object) *HTTPRouteReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add networkingv1 to scheme: %v", err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to add gatewayv1 to scheme: %v", err)
	}
	return &HTTPRouteReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
			WithInterceptorFuncs(interceptor.Funcs{Update: rejectGatewayWithoutListeners}).Build(),
		GatewayNamespace: testGatewayNamespace,
	}
}

// rejectGatewayWithoutListeners mimics the Gateway CRD's minItems=1 validation on listeners,
// which the fake client does not enforce.
func rejectGatewayWithoutListeners(
	ctx context.Context,
	c client.WithWatch,
	obj client.Object,
	opts ...client.UpdateOption,
) error {
	if gw, ok := obj.(*gatewayv1.Gateway); ok && len(gw.Spec.Listeners) == 0 {
		return errors.New("spec.listeners: should have at least 1 items")
	}
	return c.Update(ctx, obj, opts...)
}

// release runs releasePreviousGateway for the stored "api" route and returns it as persisted.
func release(t *testing.T, r *HTTPRouteReconciler, gateway string) *gatewayv1.HTTPRoute {
	t.Helper()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "app", Name: "api"}
	route := &gatewayv1.HTTPRoute{}
	if err := r.Get(ctx, key, route); err != nil {
		t.Fatalf("failed to get route: %v", err)
	}
	if err := r.releasePreviousGateway(ctx, route, gateway); err != nil {
		t.Fatalf("releasePreviousGateway() error: %v", err)
	}
	if err := r.Get(ctx, key, route); err != nil {
		t.Fatalf("failed to re-get route: %v", err)
	}
	return route
}

func getGateway(t *testing.T, r *HTTPRouteReconciler, name string) (*gatewayv1.Gateway, bool) {
	t.Helper()
	gw := &gatewayv1.Gateway{}
	err := r.Get(context.Background(), types.NamespacedName{Namespace: testGatewayNamespace, Name: name}, gw)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("failed to get Gateway %s: %v", name, err)
	}
	return gw, true
}

func TestReleasePreviousGateway_RecordsGatewayOnFirstReconcile(t *testing.T) {
	r := newReleaseReconciler(t,
		testRoute("api", "nginx", "", "api.example.com"),
		testGateway("nginx", true, "api.example.com"),
	)

	route := release(t, r, "nginx")

	if got := route.Annotations[utils.ListenerGatewayAnnotation]; got != "nginx" {
		t.Errorf("listener-gateway annotation = %q, want %q", got, "nginx")
	}
	if _, ok := getGateway(t, r, "nginx"); !ok {
		t.Error("current Gateway must not be deleted")
	}
}

func TestReleasePreviousGateway_DeletesEmptyPreviousGateway(t *testing.T) {
	r := newReleaseReconciler(t,
		testRoute("api", "app-api", "nginx", "api.example.com"),
		testGateway("nginx", true, "api.example.com"),
	)

	route := release(t, r, "app-api")

	if got := route.Annotations[utils.ListenerGatewayAnnotation]; got != "app-api" {
		t.Errorf("listener-gateway annotation = %q, want %q", got, "app-api")
	}
	if _, ok := getGateway(t, r, "nginx"); ok {
		t.Error("previous Gateway without routes should be deleted")
	}
}

func TestReleasePreviousGateway_PrunesListenersOfSharedGateway(t *testing.T) {
	r := newReleaseReconciler(t,
		testRoute("api", "app-api", "nginx", "api.example.com"),
		testRoute("web", "nginx", "nginx", "web.example.com"),
		testGateway("nginx", true, "api.example.com", "web.example.com"),
	)

	release(t, r, "app-api")

	gw, ok := getGateway(t, r, "nginx")
	if !ok {
		t.Fatal("shared Gateway still used by another route must not be deleted")
	}
	if len(gw.Spec.Listeners) != 1 || string(*gw.Spec.Listeners[0].Hostname) != "web.example.com" {
		t.Errorf("shared Gateway listeners = %v, want only web.example.com", gw.Spec.Listeners)
	}
}

func TestReleasePreviousGateway_LeavesUnmanagedGateway(t *testing.T) {
	r := newReleaseReconciler(t,
		testRoute("api", "app-api", "external", "api.example.com"),
		testGateway("external", false, "api.example.com"),
	)

	route := release(t, r, "app-api")

	if _, ok := getGateway(t, r, "external"); !ok {
		t.Error("Gateway not managed by ingress-doperator must not be deleted")
	}
	if got := route.Annotations[utils.ListenerGatewayAnnotation]; got != "app-api" {
		t.Errorf("listener-gateway annotation = %q, want %q", got, "app-api")
	}
}

func TestReleasePreviousGateway_PreviousGatewayAlreadyGone(t *testing.T) {
	r := newReleaseReconciler(t, testRoute("api", "app-api", "nginx", "api.example.com"))

	route := release(t, r, "app-api")

	if got := route.Annotations[utils.ListenerGatewayAnnotation]; got != "app-api" {
		t.Errorf("listener-gateway annotation = %q, want %q", got, "app-api")
	}
}

func TestReconcileGatewayListeners_DeletesGatewayWithoutRoutes(t *testing.T) {
	r := newReleaseReconciler(t, testGateway("app-api", true, "api.example.com"))

	updated, err := r.reconcileGatewayListeners(context.Background(), "app-api", nil)

	if err != nil {
		t.Fatalf("reconcileGatewayListeners() error: %v", err)
	}
	if !updated {
		t.Error("deleting the Gateway should report an update")
	}
	if _, ok := getGateway(t, r, "app-api"); ok {
		t.Error("Gateway without routes should be deleted instead of updated to zero listeners")
	}
}

func TestReconcileGatewayListeners_KeepsGatewayWhileRoutesLackListeners(t *testing.T) {
	// The route has no resolvable source Ingress, so its TLS is unknown and no listener can be
	// built for it; the stale listener goes, which would leave the Gateway without listeners.
	r := newReleaseReconciler(t, testGateway("app-api", true, "old.example.com"))
	routes := []gatewayv1.HTTPRoute{*testRoute("api", "app-api", "app-api", "api.example.com")}

	updated, err := r.reconcileGatewayListeners(context.Background(), "app-api", routes)

	if err != nil {
		t.Fatalf("reconcileGatewayListeners() error: %v", err)
	}
	if updated {
		t.Error("Gateway that cannot get a listener yet must not be reported as updated")
	}
	gw, ok := getGateway(t, r, "app-api")
	if !ok {
		t.Fatal("Gateway with attached routes must not be deleted")
	}
	if len(gw.Spec.Listeners) != 1 {
		t.Errorf("Gateway listeners = %v, want the stored listener left unchanged", gw.Spec.Listeners)
	}
}
