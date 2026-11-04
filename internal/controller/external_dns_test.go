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
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDisableExternalDNSUsesConfiguredPrefix(t *testing.T) {
	tests := []struct {
		prefix string
		other  string
	}{
		{prefix: DefaultExternalDNSAnnotationPrefix, other: LegacyExternalDNSAnnotationPrefix},
		{prefix: LegacyExternalDNSAnnotationPrefix, other: DefaultExternalDNSAnnotationPrefix},
	}
	t.Cleanup(func() { SetExternalDNSAnnotationPrefix(DefaultExternalDNSAnnotationPrefix) })

	for _, tt := range tests {
		t.Run(tt.prefix, func(t *testing.T) {
			SetExternalDNSAnnotationPrefix(tt.prefix)

			scheme := runtime.NewScheme()
			if err := networkingv1.AddToScheme(scheme); err != nil {
				t.Fatalf("failed to add networkingv1 to scheme: %v", err)
			}
			ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
				Namespace: "ns",
				Name:      "web",
				Annotations: map[string]string{
					tt.prefix + "/hostname": "web.example.com",
					tt.other + "/hostname":  "ignored.example.com",
				},
			}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ingress).Build()

			if err := disableExternalDNS(context.Background(), c, ingress); err != nil {
				t.Fatalf("disableExternalDNS returned error: %v", err)
			}

			got := &networkingv1.Ingress{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(ingress), got); err != nil {
				t.Fatalf("failed to get Ingress: %v", err)
			}
			if v := got.Annotations[tt.prefix+"/ingress-hostname-source"]; v != ExternalDNSHostnameSourceAnnotationOnly {
				t.Errorf("ingress-hostname-source under %q = %q, want %q",
					tt.prefix, v, ExternalDNSHostnameSourceAnnotationOnly)
			}
			if _, ok := got.Annotations[tt.other+"/ingress-hostname-source"]; ok {
				t.Errorf("unexpected ingress-hostname-source under %q", tt.other)
			}
			if v := got.Annotations[OriginalExternalDNSHostname]; v != "web.example.com" {
				t.Errorf("%s = %q, want %q", OriginalExternalDNSHostname, v, "web.example.com")
			}
		})
	}
}
