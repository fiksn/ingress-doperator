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
	"time"

	"k8s.io/apimachinery/pkg/types"
)

func TestNamedPortBackoff(t *testing.T) {
	var backoff namedPortBackoff
	first := types.NamespacedName{Namespace: "app", Name: "first"}
	other := types.NamespacedName{Namespace: "app", Name: "other"}

	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for i, seconds := range want {
		if got := backoff.next(first); got != seconds*time.Second {
			t.Errorf("attempt %d: delay = %v, want %v", i+1, got, seconds*time.Second)
		}
	}
	if got := backoff.next(other); got != time.Second {
		t.Errorf("another Ingress starts at %v, want 1s", got)
	}

	backoff.reset(first)
	if got := backoff.next(first); got != time.Second {
		t.Errorf("after reset: delay = %v, want 1s", got)
	}
}

// TestNamedPorts_RetryBacksOffAndResets checks the requeue delays of an Ingress whose Service is
// missing, and that resolving the ports resets them.
func TestNamedPorts_RetryBacksOffAndResets(t *testing.T) {
	ingress := minioShapedIngress()
	r := newTranslationReconciler(t, ingress)

	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		if got := reconcileIngress(t, r, ingress).RequeueAfter; got != want {
			t.Errorf("RequeueAfter = %v, want %v", got, want)
		}
	}

	service := storeService()
	if err := r.Create(context.Background(), service); err != nil {
		t.Fatalf("failed to create Service: %v", err)
	}
	if got := reconcileIngress(t, r, ingress).RequeueAfter; got != 0 {
		t.Fatalf("RequeueAfter after the Service exists = %v, want none", got)
	}

	if err := r.Delete(context.Background(), service); err != nil {
		t.Fatalf("failed to delete Service: %v", err)
	}
	if got := reconcileIngress(t, r, ingress).RequeueAfter; got != time.Second {
		t.Errorf("RequeueAfter after resolving once = %v, want backoff restarted at 1s", got)
	}
}
