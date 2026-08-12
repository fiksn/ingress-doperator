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

package translator

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const canarySvc = "canary-svc"

func ingressWithPath(name, host, path, service string, annotations map[string]string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "app", Annotations: annotations},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path: path,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: service,
									Port: networkingv1.ServiceBackendPort{Number: 80},
								},
							},
						}},
					},
				},
			}},
		},
	}
}

func TestIsCanary(t *testing.T) {
	if IsCanary(ingressWithPath("x", "h", "/", "s", nil)) {
		t.Fatal("ingress without annotation should not be canary")
	}
	canary := ingressWithPath("x", "h", "/", "s", map[string]string{CanaryAnnotation: "true"})
	if !IsCanary(canary) {
		t.Fatal("ingress with canary=true should be canary")
	}
	if IsCanary(nil) {
		t.Fatal("nil ingress must not be canary")
	}
}

func TestParseCanaryConfigDefaults(t *testing.T) {
	config, err := ParseCanaryConfig(ingressWithPath("c", "h", "/", "s", map[string]string{CanaryAnnotation: "true"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config.Weight != 0 || config.WeightTotal != 100 {
		t.Fatalf("expected default weight 0/100, got %d/%d", config.Weight, config.WeightTotal)
	}
}

func TestParseCanaryConfigErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"negative weight":   {CanaryWeightAnnotation: "-1"},
		"non-numeric":       {CanaryWeightAnnotation: "abc"},
		"zero total":        {CanaryWeightTotalAnnotation: "0"},
		"weight over total": {CanaryWeightAnnotation: "50", CanaryWeightTotalAnnotation: "40"},
	}
	for name, annotations := range cases {
		annotations[CanaryAnnotation] = "true"
		if _, err := ParseCanaryConfig(ingressWithPath("c", "h", "/", "s", annotations)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

// ruleForType returns the first rule whose single match uses the given path/header shape.
func weightOf(t *testing.T, ref gatewayv1.HTTPBackendRef) int32 {
	t.Helper()
	if ref.Weight == nil {
		t.Fatal("expected explicit weight, got nil")
	}
	return *ref.Weight
}

func TestCanaryWeightSplit(t *testing.T) {
	tr := New(Config{GatewayName: "gw", GatewayNamespace: "gw"})
	primary := ingressWithPath("prod", "app.example.com", "/", "prod-svc", nil)
	canary := ingressWithPath("canary", "app.example.com", "/", canarySvc, map[string]string{
		CanaryAnnotation:       "true",
		CanaryWeightAnnotation: "20",
	})

	route := tr.TranslateToHTTPRouteWithCanaries(primary, []*networkingv1.Ingress{canary})

	if len(route.Spec.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(route.Spec.Rules))
	}
	refs := route.Spec.Rules[0].BackendRefs
	if len(refs) != 2 {
		t.Fatalf("expected 2 backendRefs, got %d", len(refs))
	}
	if string(refs[0].Name) != "prod-svc" || weightOf(t, refs[0]) != 80 {
		t.Errorf("primary backend: got %s weight %v, want prod-svc/80", refs[0].Name, refs[0].Weight)
	}
	if string(refs[1].Name) != canarySvc || weightOf(t, refs[1]) != 20 {
		t.Errorf("canary backend: got %s weight %v, want canary-svc/20", refs[1].Name, refs[1].Weight)
	}
}

func TestCanaryWeightTotal(t *testing.T) {
	tr := New(Config{})
	primary := ingressWithPath("prod", "h", "/", "prod-svc", nil)
	canary := ingressWithPath("canary", "h", "/", canarySvc, map[string]string{
		CanaryAnnotation:            "true",
		CanaryWeightAnnotation:      "200",
		CanaryWeightTotalAnnotation: "1000",
	})

	route := tr.TranslateToHTTPRouteWithCanaries(primary, []*networkingv1.Ingress{canary})
	refs := route.Spec.Rules[0].BackendRefs
	if weightOf(t, refs[0]) != 800 || weightOf(t, refs[1]) != 200 {
		t.Fatalf("expected 800/200 split, got %v/%v", refs[0].Weight, refs[1].Weight)
	}
}

func TestCanaryByHeaderDefault(t *testing.T) {
	tr := New(Config{})
	primary := ingressWithPath("prod", "h", "/", "prod-svc", nil)
	canary := ingressWithPath("canary", "h", "/", canarySvc, map[string]string{
		CanaryAnnotation:         "true",
		CanaryByHeaderAnnotation: "X-Canary",
	})

	route := tr.TranslateToHTTPRouteWithCanaries(primary, []*networkingv1.Ingress{canary})

	// Expect: never->primary, always->canary, then the weight split.
	if len(route.Spec.Rules) != 3 {
		t.Fatalf("expected 3 rules, got %d", len(route.Spec.Rules))
	}
	never := route.Spec.Rules[0]
	assertHeaderMatch(t, never, "X-Canary", gatewayv1.HeaderMatchExact, "never")
	if string(never.BackendRefs[0].Name) != "prod-svc" {
		t.Errorf("never rule should route to primary, got %s", never.BackendRefs[0].Name)
	}
	always := route.Spec.Rules[1]
	assertHeaderMatch(t, always, "X-Canary", gatewayv1.HeaderMatchExact, "always")
	if len(always.BackendRefs) != 1 || string(always.BackendRefs[0].Name) != canarySvc {
		t.Errorf("always rule should route only to canary, got %+v", always.BackendRefs)
	}
	if always.BackendRefs[0].Weight != nil {
		t.Errorf("canary-only backend should have no weight, got %v", *always.BackendRefs[0].Weight)
	}
}

func TestCanaryByHeaderValue(t *testing.T) {
	tr := New(Config{})
	primary := ingressWithPath("prod", "h", "/", "prod-svc", nil)
	canary := ingressWithPath("canary", "h", "/", canarySvc, map[string]string{
		CanaryAnnotation:              "true",
		CanaryByHeaderAnnotation:      "X-Region",
		CanaryByHeaderValueAnnotation: "eu",
	})

	route := tr.TranslateToHTTPRouteWithCanaries(primary, []*networkingv1.Ingress{canary})
	// No "never" rule when an explicit value is set: header->canary, then weight split.
	if len(route.Spec.Rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(route.Spec.Rules))
	}
	assertHeaderMatch(t, route.Spec.Rules[0], "X-Region", gatewayv1.HeaderMatchExact, "eu")
}

func TestCanaryByHeaderPattern(t *testing.T) {
	tr := New(Config{})
	primary := ingressWithPath("prod", "h", "/", "prod-svc", nil)
	canary := ingressWithPath("canary", "h", "/", canarySvc, map[string]string{
		CanaryAnnotation:                "true",
		CanaryByHeaderAnnotation:        "X-Region",
		CanaryByHeaderPatternAnnotation: "eu-.*",
	})

	route := tr.TranslateToHTTPRouteWithCanaries(primary, []*networkingv1.Ingress{canary})
	if len(route.Spec.Rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(route.Spec.Rules))
	}
	assertHeaderMatch(t, route.Spec.Rules[0], "X-Region", gatewayv1.HeaderMatchRegularExpression, "eu-.*")
}

func TestCanaryByCookie(t *testing.T) {
	tr := New(Config{})
	primary := ingressWithPath("prod", "h", "/", "prod-svc", nil)
	canary := ingressWithPath("canary", "h", "/", canarySvc, map[string]string{
		CanaryAnnotation:         "true",
		CanaryByCookieAnnotation: "canary_user",
	})

	route := tr.TranslateToHTTPRouteWithCanaries(primary, []*networkingv1.Ingress{canary})
	if len(route.Spec.Rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(route.Spec.Rules))
	}
	cookieRule := route.Spec.Rules[0]
	assertHeaderMatch(t, cookieRule, "Cookie", gatewayv1.HeaderMatchRegularExpression,
		`(?:^|;\s*)canary_user=always(?:;|$)`)
	if string(cookieRule.BackendRefs[0].Name) != canarySvc {
		t.Errorf("cookie rule should route to canary, got %s", cookieRule.BackendRefs[0].Name)
	}
}

func TestNoCanaryProducesSingleBackend(t *testing.T) {
	tr := New(Config{})
	primary := ingressWithPath("prod", "h", "/", "prod-svc", nil)

	route := tr.TranslateToHTTPRoute(primary)
	if len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].BackendRefs) != 1 {
		t.Fatalf("expected 1 rule with 1 backend, got %+v", route.Spec.Rules)
	}
	if route.Spec.Rules[0].BackendRefs[0].Weight != nil {
		t.Errorf("non-canary backend should not carry a weight")
	}
}

func TestCanaryOnlyMatchesSharedPath(t *testing.T) {
	tr := New(Config{})
	primary := ingressWithPath("prod", "h", "/api", "prod-svc", nil)
	canary := ingressWithPath("canary", "h", "/other", canarySvc, map[string]string{
		CanaryAnnotation:       "true",
		CanaryWeightAnnotation: "20",
	})

	// Canary path does not match the primary path; entries are keyed by path.
	route := tr.TranslateToHTTPRouteWithCanaries(primary, []*networkingv1.Ingress{canary})
	if len(route.Spec.Rules[0].BackendRefs) != 1 {
		t.Fatalf("mismatched path must not fold in canary backend, got %+v", route.Spec.Rules[0].BackendRefs)
	}
}

func assertHeaderMatch(
	t *testing.T,
	rule gatewayv1.HTTPRouteRule,
	name string,
	matchType gatewayv1.HeaderMatchType,
	value string,
) {
	t.Helper()
	if len(rule.Matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(rule.Matches))
	}
	headers := rule.Matches[0].Headers
	if len(headers) != 1 {
		t.Fatalf("expected 1 header match, got %d", len(headers))
	}
	header := headers[0]
	if string(header.Name) != name {
		t.Errorf("header name: got %s, want %s", header.Name, name)
	}
	if header.Type == nil || *header.Type != matchType {
		t.Errorf("header match type: got %v, want %s", header.Type, matchType)
	}
	if header.Value != value {
		t.Errorf("header value: got %q, want %q", header.Value, value)
	}
}
