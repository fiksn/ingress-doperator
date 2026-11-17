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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/translator"
)

// secureIngress is an Ingress named name using Service "store" on the named port "api" over HTTPS.
func secureIngress(name string, annotations map[string]string) *networkingv1.Ingress {
	ingress := multiHostIngress(hostRule(name+".example.com",
		ingressPath("/", networkingv1.PathTypePrefix, "store", namedPort("api"))))
	ingress.Name = name
	ingress.UID = types.UID(name + "-uid")
	ingress.Annotations = map[string]string{"nginx.ingress.kubernetes.io/backend-protocol": "HTTPS"}
	for key, value := range annotations {
		ingress.Annotations[key] = value
	}
	return ingress
}

// getPolicy returns the BackendTLSPolicy of Service "store".
func getPolicy(t *testing.T, r *IngressReconciler) (*gatewayv1.BackendTLSPolicy, bool) {
	t.Helper()
	policy := &gatewayv1.BackendTLSPolicy{}
	key := types.NamespacedName{Namespace: multiHostNamespace, Name: backendTLSPolicyName("store")}
	err := r.Get(context.Background(), key, policy)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("failed to get BackendTLSPolicy: %v", err)
	}
	return policy, true
}

func drainEvents(recorder *events.FakeRecorder) string {
	var all []string
	for {
		select {
		case event := <-recorder.Events:
			all = append(all, event)
		default:
			return strings.Join(all, "\n")
		}
	}
}

func TestBackendTLSPolicy_GeneratedForService(t *testing.T) {
	ingress := secureIngress("objects", map[string]string{translator.BackendTLSCAAnnotation: "Secret/store-ca"})
	r := newTranslationReconciler(t, ingress, storeService())

	reconcileIngress(t, r, ingress)

	policy, ok := getPolicy(t, r)
	if !ok {
		t.Fatal("no BackendTLSPolicy generated")
	}
	if got := policy.Annotations[translator.ManagedByAnnotation]; got != translator.ManagedByValue {
		t.Errorf("managed-by = %q", got)
	}
	if got := policy.Annotations[translator.SourceAnnotation]; got != "storage/objects" {
		t.Errorf("source = %q, want storage/objects", got)
	}
	if len(policy.Spec.TargetRefs) != 1 || policy.Spec.TargetRefs[0].Name != "store" ||
		policy.Spec.TargetRefs[0].Kind != "Service" || ptrValue(policy.Spec.TargetRefs[0].SectionName) != "api" {
		t.Errorf("targetRefs = %+v, want Service store section api", policy.Spec.TargetRefs)
	}
	validation := policy.Spec.Validation
	if validation.Hostname != "store.storage.svc" {
		t.Errorf("hostname = %q, want store.storage.svc", validation.Hostname)
	}
	if len(validation.CACertificateRefs) != 1 || validation.CACertificateRefs[0].Kind != "Secret" ||
		validation.CACertificateRefs[0].Name != "store-ca" || validation.WellKnownCACertificates != nil {
		t.Errorf("CA = %+v / %v, want Secret store-ca", validation.CACertificateRefs, validation.WellKnownCACertificates)
	}
}

func TestBackendTLSPolicy_SystemCAsAndWarnings(t *testing.T) {
	ingress := secureIngress("objects", map[string]string{
		translator.ProxySSLVerifyAnnotation: "off",
		translator.ProxySSLNameAnnotation:   "store.internal.example",
	})
	recorder := events.NewFakeRecorder(20)
	r := newTranslationReconciler(t, ingress, storeService())
	r.Recorder = recorder

	reconcileIngress(t, r, ingress)

	policy, ok := getPolicy(t, r)
	if !ok {
		t.Fatal("no BackendTLSPolicy generated")
	}
	if policy.Spec.Validation.WellKnownCACertificates == nil ||
		*policy.Spec.Validation.WellKnownCACertificates != gatewayv1.WellKnownCACertificatesSystem {
		t.Errorf("wellKnownCACertificates = %v, want System", policy.Spec.Validation.WellKnownCACertificates)
	}
	if policy.Spec.Validation.Hostname != "store.internal.example" {
		t.Errorf("hostname = %q, want the proxy-ssl-name", policy.Spec.Validation.Hostname)
	}
	got := drainEvents(recorder)
	for _, want := range []string{"Warning BackendTLSSetting", "system CAs", "always verifies"} {
		if !strings.Contains(got, want) {
			t.Errorf("events do not mention %q:\n%s", want, got)
		}
	}
}

func TestBackendTLSPolicy_ForeignPolicyIsLeftAlone(t *testing.T) {
	ingress := secureIngress("objects", nil)
	foreign := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: multiHostNamespace, Name: "hand-made"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{{
				LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Kind: "Service", Name: "store"},
			}},
			Validation: gatewayv1.BackendTLSPolicyValidation{Hostname: "store.example"},
		},
	}
	recorder := events.NewFakeRecorder(20)
	r := newTranslationReconciler(t, ingress, storeService(), foreign)
	r.Recorder = recorder

	reconcileIngress(t, r, ingress)

	if _, ok := getPolicy(t, r); ok {
		t.Error("generated a BackendTLSPolicy although Service store already has one")
	}
	if got := drainEvents(recorder); !strings.Contains(got, "BackendTLSPolicyNotManaged") ||
		!strings.Contains(got, "hand-made") {
		t.Errorf("no warning naming the foreign policy:\n%s", got)
	}
}

func TestBackendTLSPolicy_SharedAndReleased(t *testing.T) {
	first := secureIngress("alpha", map[string]string{translator.BackendTLSCAAnnotation: "Secret/store-ca"})
	second := secureIngress("beta", map[string]string{translator.BackendTLSCAAnnotation: "Secret/store-ca"})
	r := newTranslationReconciler(t, first, second, storeService())
	reconcileIngress(t, r, first)
	reconcileIngress(t, r, second)

	policy, ok := getPolicy(t, r)
	if !ok || policy.Annotations[translator.SourceAnnotation] != "storage/alpha,storage/beta" {
		t.Fatalf("shared policy = %+v, want sources storage/alpha,storage/beta", policy)
	}

	dropHTTPS := func(ingress *networkingv1.Ingress) {
		t.Helper()
		latest := &networkingv1.Ingress{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(ingress), latest); err != nil {
			t.Fatalf("failed to get Ingress: %v", err)
		}
		delete(latest.Annotations, "nginx.ingress.kubernetes.io/backend-protocol")
		if err := r.Update(context.Background(), latest); err != nil {
			t.Fatalf("failed to update Ingress: %v", err)
		}
		reconcileIngress(t, r, ingress)
	}

	dropHTTPS(first)
	if policy, ok := getPolicy(t, r); !ok || policy.Annotations[translator.SourceAnnotation] != "storage/beta" {
		t.Fatalf("after releasing alpha, policy = %+v, want sources storage/beta", policy)
	}
	dropHTTPS(second)
	if _, ok := getPolicy(t, r); ok {
		t.Error("policy still exists after no Ingress needs it")
	}
}

func TestBackendTLSPolicy_ConflictKeepsFirstSettings(t *testing.T) {
	first := secureIngress("alpha", map[string]string{translator.BackendTLSCAAnnotation: "Secret/alpha-ca"})
	second := secureIngress("beta", map[string]string{translator.BackendTLSCAAnnotation: "Secret/beta-ca"})
	recorder := events.NewFakeRecorder(20)
	r := newTranslationReconciler(t, first, second, storeService())
	r.Recorder = recorder

	reconcileIngress(t, r, first)
	reconcileIngress(t, r, second)
	reconcileIngress(t, r, first)

	policy, _ := getPolicy(t, r)
	if refs := policy.Spec.Validation.CACertificateRefs; len(refs) != 1 || refs[0].Name != "alpha-ca" {
		t.Errorf("CA = %+v, want alpha-ca kept", refs)
	}
	if got := drainEvents(recorder); !strings.Contains(got, "BackendTLSPolicyConflict") || !strings.Contains(got, "beta") {
		t.Errorf("no conflict warning for beta:\n%s", got)
	}
}

func TestBackendTLSPolicy_Targets(t *testing.T) {
	unnamed := storeService()
	unnamed.Spec.Ports = []corev1.ServicePort{{Port: 443}}
	tests := []struct {
		name    string
		port    networkingv1.ServiceBackendPort
		service *corev1.Service
		want    string
	}{
		{"named port", namedPort("console"), storeService(), "console"},
		{"numbered port resolved to its name", numberedPort(apiPortNumber), storeService(), "api"},
		{"port without a name targets the Service", numberedPort(443), unnamed, ""},
		{"missing Service targets the Service", numberedPort(443), nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ingress := multiHostIngress(hostRule(consoleHost, ingressPath("/", networkingv1.PathTypePrefix, "store", tt.port)))
			ingress.Annotations = map[string]string{"nginx.ingress.kubernetes.io/backend-protocol": "HTTPS"}
			objs := []client.Object{ingress}
			if tt.service != nil {
				objs = append(objs, tt.service)
			}
			r := newTranslationReconciler(t, objs...)

			reconcileIngress(t, r, ingress)

			policy, ok := getPolicy(t, r)
			if !ok {
				t.Fatal("no BackendTLSPolicy generated")
			}
			if len(policy.Spec.TargetRefs) != 1 || ptrValue(policy.Spec.TargetRefs[0].SectionName) != tt.want {
				t.Errorf("targetRefs = %+v, want one with section %q", policy.Spec.TargetRefs, tt.want)
			}
		})
	}
}

func TestBackendTLSPolicy_ReleasedOnIngressDeletion(t *testing.T) {
	ingress := secureIngress("objects", nil)
	ingress.Finalizers = []string{FinalizerName}
	r := newTranslationReconciler(t, ingress, storeService())
	r.EnableDeletion = true
	reconcileIngress(t, r, ingress)
	if _, ok := getPolicy(t, r); !ok {
		t.Fatal("no BackendTLSPolicy generated")
	}

	if err := r.Delete(context.Background(), ingress); err != nil {
		t.Fatalf("failed to delete Ingress: %v", err)
	}
	reconcileIngress(t, r, ingress)

	if _, ok := getPolicy(t, r); ok {
		t.Error("BackendTLSPolicy still exists after its only Ingress was deleted")
	}
}

func ptrValue(section *gatewayv1.SectionName) string {
	if section == nil {
		return ""
	}
	return string(*section)
}

// TestBackendTLSPolicy_ExistingSettingsAreKept checks that a later Ingress with different settings
// does not take the policy over, even when it sorts first.
func TestBackendTLSPolicy_ExistingSettingsAreKept(t *testing.T) {
	established := secureIngress("zulu", map[string]string{translator.BackendTLSCAAnnotation: "Secret/zulu-ca"})
	r := newTranslationReconciler(t, established, storeService())
	reconcileIngress(t, r, established)

	newcomer := secureIngress("alpha", map[string]string{translator.BackendTLSCAAnnotation: "Secret/alpha-ca"})
	if err := r.Create(context.Background(), newcomer); err != nil {
		t.Fatalf("failed to create Ingress: %v", err)
	}
	reconcileIngress(t, r, newcomer)
	reconcileIngress(t, r, established)

	policy, _ := getPolicy(t, r)
	if refs := policy.Spec.Validation.CACertificateRefs; len(refs) != 1 || refs[0].Name != "zulu-ca" {
		t.Errorf("CA = %+v, want the established zulu-ca kept", refs)
	}
	if got := policy.Annotations[translator.SourceAnnotation]; got != "storage/alpha,storage/zulu" {
		t.Errorf("source = %q, want both Ingresses", got)
	}
}
