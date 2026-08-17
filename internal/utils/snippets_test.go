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

package utils

import (
	"strings"
	"testing"
)

const authPrefix = "nginx.ingress.kubernetes.io/"

// snippetValue returns the value emitted for the given SnippetsFilter context.
func snippetValue(snippets []map[string]interface{}, context string) string {
	for _, snippet := range snippets {
		if snippet["context"] == context {
			if value, ok := snippet["value"].(string); ok {
				return value
			}
		}
	}
	return ""
}

func containsWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func TestBuildAuthSnippets_Minimal(t *testing.T) {
	annotations := map[string]string{
		authPrefix + "auth-url": "https://auth.example.com/oauth2/auth",
	}
	snippets, _, ok := BuildNginxIngressSnippets(annotations, AuthInputs{Identifier: "team-app"}, nil)
	if !ok {
		t.Fatal("expected snippets to be produced")
	}

	location := snippetValue(snippets, "http.server.location")
	if !strings.Contains(location, "auth_request /_doperator_auth_team_app;") {
		t.Errorf("location context missing auth_request directive: %q", location)
	}

	server := snippetValue(snippets, "http.server")
	for _, want := range []string{
		"location = /_doperator_auth_team_app {",
		"internal;",
		"proxy_set_header X-Original-URL $scheme://$http_host$request_uri;",
		"proxy_set_header Host auth.example.com;",
		"proxy_ssl_server_name on;",
		"proxy_pass https://auth.example.com/oauth2/auth;",
	} {
		if !strings.Contains(server, want) {
			t.Errorf("server context missing %q in:\n%s", want, server)
		}
	}
}

func TestBuildAuthSnippets_Signin(t *testing.T) {
	annotations := map[string]string{
		authPrefix + "auth-url":    "https://auth.example.com/oauth2/auth",
		authPrefix + "auth-signin": "https://auth.example.com/oauth2/start?rd=$escaped_request_uri",
	}
	snippets, _, ok := BuildNginxIngressSnippets(annotations, AuthInputs{Identifier: "ns-name"}, nil)
	if !ok {
		t.Fatal("expected snippets")
	}

	location := snippetValue(snippets, "http.server.location")
	if !strings.Contains(location, "error_page 401 = @doperator_signin_ns_name;") {
		t.Errorf("location missing error_page redirect: %q", location)
	}

	server := snippetValue(snippets, "http.server")
	if !strings.Contains(server, "location @doperator_signin_ns_name {") {
		t.Errorf("server missing signin location: %q", server)
	}
	if !strings.Contains(server, "return 302 https://auth.example.com/oauth2/start?rd=$request_uri;") {
		t.Errorf("escaped_request_uri not substituted with request_uri: %q", server)
	}
	if strings.Contains(server, "$escaped_request_uri") {
		t.Errorf("server still references $escaped_request_uri: %q", server)
	}
}

func TestBuildAuthSnippets_SigninRedirectParamAppended(t *testing.T) {
	annotations := map[string]string{
		authPrefix + "auth-url":                   "https://auth.example.com/auth",
		authPrefix + "auth-signin":                "https://auth.example.com/start",
		authPrefix + "auth-signin-redirect-param": "next",
	}
	snippets, _, _ := BuildNginxIngressSnippets(annotations, AuthInputs{Identifier: "x"}, nil)
	server := snippetValue(snippets, "http.server")
	if !strings.Contains(server, "return 302 https://auth.example.com/start?next=$request_uri;") {
		t.Errorf("redirect param not appended: %q", server)
	}
}

func TestBuildAuthSnippets_ResponseHeaders(t *testing.T) {
	annotations := map[string]string{
		authPrefix + "auth-url":              "https://auth.example.com/auth",
		authPrefix + "auth-response-headers": "X-Auth-Request-User, X-Auth-Request-Email",
	}
	snippets, _, _ := BuildNginxIngressSnippets(annotations, AuthInputs{Identifier: "x"}, nil)
	location := snippetValue(snippets, "http.server.location")
	for _, want := range []string{
		"auth_request_set $doperator_auth_h0 $upstream_http_x_auth_request_user;",
		"proxy_set_header X-Auth-Request-User $doperator_auth_h0;",
		"auth_request_set $doperator_auth_h1 $upstream_http_x_auth_request_email;",
		"proxy_set_header X-Auth-Request-Email $doperator_auth_h1;",
	} {
		if !strings.Contains(location, want) {
			t.Errorf("location missing %q in:\n%s", want, location)
		}
	}
}

func TestBuildAuthSnippets_Keepalive(t *testing.T) {
	annotations := map[string]string{
		authPrefix + "auth-url":       "https://auth.example.com:8443/auth",
		authPrefix + "auth-keepalive": "32",
	}
	snippets, _, _ := BuildNginxIngressSnippets(annotations, AuthInputs{Identifier: "x"}, nil)
	httpCtx := snippetValue(snippets, "http")
	if !strings.Contains(httpCtx, "upstream doperator_auth_x {") ||
		!strings.Contains(httpCtx, "server auth.example.com:8443;") ||
		!strings.Contains(httpCtx, "keepalive 32;") {
		t.Errorf("http context missing keepalive upstream: %q", httpCtx)
	}
	server := snippetValue(snippets, "http.server")
	if !strings.Contains(server, "proxy_pass https://doperator_auth_x/auth;") {
		t.Errorf("auth location should proxy_pass to upstream: %q", server)
	}
	if !strings.Contains(server, "proxy_http_version 1.1;") {
		t.Errorf("keepalive requires proxy_http_version 1.1: %q", server)
	}
}

func TestBuildAuthSnippets_Caching(t *testing.T) {
	annotations := map[string]string{
		authPrefix + "auth-url":            "https://auth.example.com/auth",
		authPrefix + "auth-cache-key":      "$remote_user$http_authorization",
		authPrefix + "auth-cache-duration": "200 202 5m",
	}
	snippets, _, _ := BuildNginxIngressSnippets(annotations, AuthInputs{Identifier: "x"}, nil)
	httpCtx := snippetValue(snippets, "http")
	if !strings.Contains(httpCtx, "proxy_cache_path /var/cache/nginx/doperator_auth_x") {
		t.Errorf("http context missing proxy_cache_path: %q", httpCtx)
	}
	server := snippetValue(snippets, "http.server")
	if !strings.Contains(server, "proxy_cache doperator_auth_x;") ||
		!strings.Contains(server, "proxy_cache_valid 200 202 5m;") {
		t.Errorf("auth location missing cache directives: %q", server)
	}
}

func TestBuildAuthSnippets_ProxySetHeaders(t *testing.T) {
	annotations := map[string]string{
		authPrefix + "auth-url": "https://auth.example.com/auth",
	}
	inputs := AuthInputs{
		Identifier:      "x",
		ProxySetHeaders: map[string]string{"X-Custom": "value", "X-Other": "thing"},
	}
	snippets, _, _ := BuildNginxIngressSnippets(annotations, inputs, nil)
	server := snippetValue(snippets, "http.server")
	if !strings.Contains(server, `proxy_set_header X-Custom "value";`) ||
		!strings.Contains(server, `proxy_set_header X-Other "thing";`) {
		t.Errorf("auth location missing resolved proxy-set-headers: %q", server)
	}
}

func TestBuildAuthSnippets_RejectsUnsafeURL(t *testing.T) {
	annotations := map[string]string{
		authPrefix + "auth-url": "https://auth.example.com/auth; return 200",
	}
	snippets, warnings, ok := BuildNginxIngressSnippets(annotations, AuthInputs{Identifier: "x"}, nil)
	if ok {
		t.Errorf("expected no snippets for unsafe auth-url, got %v", snippets)
	}
	if !containsWarning(warnings, "auth-url") {
		t.Errorf("expected warning about invalid auth-url, got %v", warnings)
	}
}

func TestBuildAuthSnippets_RejectsNonHTTPScheme(t *testing.T) {
	annotations := map[string]string{
		authPrefix + "auth-url": "file:///etc/passwd",
	}
	_, warnings, ok := BuildNginxIngressSnippets(annotations, AuthInputs{Identifier: "x"}, nil)
	if ok {
		t.Error("expected non-http scheme auth-url to be rejected")
	}
	if !containsWarning(warnings, "auth-url") {
		t.Errorf("expected warning, got %v", warnings)
	}
}

func TestBuildAuthSnippets_InvalidMethodWarns(t *testing.T) {
	annotations := map[string]string{
		authPrefix + "auth-url":    "https://auth.example.com/auth",
		authPrefix + "auth-method": "FETCH",
	}
	snippets, warnings, _ := BuildNginxIngressSnippets(annotations, AuthInputs{Identifier: "x"}, nil)
	server := snippetValue(snippets, "http.server")
	if strings.Contains(server, "proxy_method") {
		t.Errorf("invalid method should not emit proxy_method: %q", server)
	}
	if !containsWarning(warnings, "auth-method") {
		t.Errorf("expected auth-method warning, got %v", warnings)
	}
}

func TestBuildAuthSnippets_ValidMethod(t *testing.T) {
	annotations := map[string]string{
		authPrefix + "auth-url":    "https://auth.example.com/auth",
		authPrefix + "auth-method": "post",
	}
	snippets, _, _ := BuildNginxIngressSnippets(annotations, AuthInputs{Identifier: "x"}, nil)
	server := snippetValue(snippets, "http.server")
	if !strings.Contains(server, "proxy_method POST;") {
		t.Errorf("expected proxy_method POST, got: %q", server)
	}
}

func TestBuildAuthSnippets_RejectsBadResponseHeader(t *testing.T) {
	annotations := map[string]string{
		authPrefix + "auth-url":              "https://auth.example.com/auth",
		authPrefix + "auth-response-headers": "X-Good, bad header!",
	}
	snippets, warnings, _ := BuildNginxIngressSnippets(annotations, AuthInputs{Identifier: "x"}, nil)
	location := snippetValue(snippets, "http.server.location")
	if !strings.Contains(location, "$upstream_http_x_good") {
		t.Errorf("valid header should be emitted: %q", location)
	}
	if !containsWarning(warnings, "auth-response-headers") {
		t.Errorf("expected warning for invalid header, got %v", warnings)
	}
}

func TestSanitizeNginxIdentifier(t *testing.T) {
	cases := map[string]string{
		"team-app":        "team_app",
		"NS/Name.v2":      "ns_name_v2",
		"___":             "default",
		"":                "default",
		"already_ok_1234": "already_ok_1234",
	}
	for in, want := range cases {
		if got := sanitizeNginxIdentifier(in); got != want {
			t.Errorf("sanitizeNginxIdentifier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildNginxIngressSnippets_NoAuthNoOtherAnnotations(t *testing.T) {
	_, _, ok := BuildNginxIngressSnippets(map[string]string{}, AuthInputs{Identifier: "x"}, nil)
	if ok {
		t.Error("expected ok=false when there are no relevant annotations")
	}
}

func TestBuildNginxIngressSnippets_ProxyDirectivesInLocationContext(t *testing.T) {
	annotations := map[string]string{
		authPrefix + "proxy-read-timeout": "60s",
		authPrefix + "proxy-body-size":    "10m",
	}
	snippets, _, ok := BuildNginxIngressSnippets(annotations, AuthInputs{Identifier: "x"}, nil)
	if !ok {
		t.Fatal("expected snippets")
	}
	location := snippetValue(snippets, "http.server.location")
	for _, want := range []string{"proxy_read_timeout 60s;", "client_max_body_size 10m;"} {
		if !strings.Contains(location, want) {
			t.Errorf("location context missing %q in:\n%s", want, location)
		}
	}
	if server := snippetValue(snippets, "http.server"); strings.Contains(server, "proxy_read_timeout") {
		t.Errorf("proxy_read_timeout must not be in server context: %q", server)
	}
}

func TestBuildNginxIngressSnippets_SSLCiphersStayInServer(t *testing.T) {
	annotations := map[string]string{authPrefix + "ssl-ciphers": "HIGH:!aNULL"}

	snippets, _, ok := BuildNginxIngressSnippets(annotations, AuthInputs{Identifier: "x"}, nil)
	if !ok {
		t.Fatal("expected snippets")
	}
	if server := snippetValue(snippets, "http.server"); !strings.Contains(server, "ssl_ciphers HIGH:!aNULL;") {
		t.Errorf("ssl_ciphers should stay in server context: %q", server)
	}
	if location := snippetValue(snippets, "http.server.location"); strings.Contains(location, "ssl_ciphers") {
		t.Errorf("ssl_ciphers must not appear in location context: %q", location)
	}

	// Suppressing the directive (older Ingress owns it) drops it entirely.
	suppressed, _, ok := BuildNginxIngressSnippets(
		annotations, AuthInputs{Identifier: "x"}, map[string]struct{}{"ssl-ciphers": {}})
	if ok {
		t.Errorf("expected no snippets once the only directive is suppressed, got %v", suppressed)
	}
}

func TestBuildNginxIngressSnippets_CustomErrorPerIngressLocation(t *testing.T) {
	annotations := map[string]string{authPrefix + "custom-http-errors": "404"}
	snippets, _, ok := BuildNginxIngressSnippets(annotations, AuthInputs{Identifier: "ns-app"}, nil)
	if !ok {
		t.Fatal("expected snippets")
	}
	location := snippetValue(snippets, "http.server.location")
	if !strings.Contains(location, "proxy_intercept_errors on;") ||
		!strings.Contains(location, "error_page 404 = @ingress_doperator_error_ns_app;") {
		t.Errorf("location context missing per-Ingress error_page: %q", location)
	}
	server := snippetValue(snippets, "http.server")
	if !strings.Contains(server, "location @ingress_doperator_error_ns_app {") {
		t.Errorf("server context missing per-Ingress named error location: %q", server)
	}
}
