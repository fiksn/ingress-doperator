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

// featureSnippets builds the snippets for ingress-nginx annotations given without their prefix and
// returns the http, server and location contexts.
func featureSnippets(t *testing.T, annotations map[string]string) (httpCtx, server, location string, warnings []string) {
	t.Helper()
	prefixed := make(map[string]string, len(annotations))
	for key, value := range annotations {
		prefixed[authPrefix+key] = value
	}
	snippets, warnings, _ := BuildNginxIngressSnippets(prefixed, AuthInputs{Identifier: "team-app"}, nil)
	return snippetValue(snippets, "http"), snippetValue(snippets, "http.server"),
		snippetValue(snippets, "http.server.location"), warnings
}

func assertContainsAll(t *testing.T, context, got string, want []string) {
	t.Helper()
	for _, line := range want {
		if !strings.Contains(got, line) {
			t.Errorf("%s does not contain %q:\n%s", context, line, got)
		}
	}
}

func TestCORS_DisabledUnlessEnabled(t *testing.T) {
	for _, enable := range []string{"", "false", "yes"} {
		_, _, location, _ := featureSnippets(t, map[string]string{
			"enable-cors": enable, "cors-allow-origin": "https://app.example.com",
		})
		if strings.Contains(location, "Access-Control") {
			t.Errorf("enable-cors=%q emitted CORS headers:\n%s", enable, location)
		}
	}
}

func TestCORS_Defaults(t *testing.T) {
	_, _, location, warnings := featureSnippets(t, map[string]string{"enable-cors": "true"})

	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	assertContainsAll(t, "location", location, []string{
		`set $doperator_cors_team_app true;`,
		`set $doperator_cors_allow_origin_team_app "*";`,
		`set $doperator_cors_allow_credentials_team_app "true";`,
		`set $doperator_cors_allow_methods_team_app "GET, PUT, POST, DELETE, PATCH, OPTIONS";`,
		`set $doperator_cors_allow_headers_team_app "` + defaultCORSHeaders + `";`,
		`set $doperator_cors_max_age_team_app "1728000";`,
		`if ($request_method = OPTIONS) { set $doperator_cors_team_app "${doperator_cors_team_app}options"; }`,
		`if ($doperator_cors_team_app = trueoptions) { return 204; }`,
		`proxy_hide_header Access-Control-Allow-Origin;`,
		`add_header Access-Control-Allow-Origin $doperator_cors_allow_origin_team_app always;`,
		`add_header Access-Control-Max-Age $doperator_cors_max_age_team_app always;`,
	})
	if strings.Contains(location, "$http_origin ~*") || strings.Contains(location, "Expose-Headers") {
		t.Errorf("any-origin CORS should neither match the origin nor expose headers:\n%s", location)
	}
}

func TestCORS_OriginList(t *testing.T) {
	_, _, location, warnings := featureSnippets(t, map[string]string{
		"enable-cors":            "true",
		"cors-allow-origin":      "https://app.example.com, https://*.example.org:8443, bad origin",
		"cors-allow-credentials": "false",
		"cors-expose-headers":    "X-Total, X-Page",
		"cors-max-age":           "600",
	})

	if !containsWarning(warnings, `invalid origin "bad origin"`) {
		t.Errorf("missing warning for the invalid origin: %v", warnings)
	}
	assertContainsAll(t, "location", location, []string{
		`if ($http_origin ~* "^(https://app\.example\.com|https://[A-Za-z0-9\-]+\.example\.org:8443)$") {`,
		`    set $doperator_cors_allow_origin_team_app "$http_origin";`,
		`    set $doperator_cors_expose_headers_team_app "X-Total, X-Page";`,
		`    set $doperator_cors_max_age_team_app "600";`,
	})
	unconditional := strings.SplitN(location, "if ($http_origin", 2)[0]
	if strings.Contains(unconditional, "set $doperator_cors_team_app true;") {
		t.Errorf("CORS enabled for every origin:\n%s", location)
	}
	if strings.Contains(location, "Allow-Credentials") {
		t.Errorf("credentials header emitted with cors-allow-credentials=false:\n%s", location)
	}
}

func TestCORS_InvalidValuesFallBackToDefaults(t *testing.T) {
	_, _, location, warnings := featureSnippets(t, map[string]string{
		"enable-cors":            "true",
		"cors-allow-methods":     "GET; return 200",
		"cors-allow-headers":     `X-A"`,
		"cors-allow-credentials": "maybe",
		"cors-max-age":           "1h",
	})

	for _, key := range []string{"cors-allow-methods", "cors-allow-headers", "cors-allow-credentials", "cors-max-age"} {
		if !containsWarning(warnings, key) {
			t.Errorf("missing warning for invalid %s: %v", key, warnings)
		}
	}
	assertContainsAll(t, "location", location, []string{
		`"` + defaultCORSMethods + `"`, `"` + defaultCORSHeaders + `"`, `"1728000"`,
		`set $doperator_cors_allow_credentials_team_app "true";`,
	})
	if strings.Contains(location, "return 200") || strings.Contains(location, `X-A"`) {
		t.Errorf("invalid value reached the configuration:\n%s", location)
	}
}

func TestCORS_NoValidOriginIsNotTranslated(t *testing.T) {
	_, _, location, warnings := featureSnippets(t, map[string]string{
		"enable-cors": "true", "cors-allow-origin": "https://a.example.com/path",
	})

	if strings.Contains(location, "Access-Control") {
		t.Errorf("CORS translated without a valid origin:\n%s", location)
	}
	if !containsWarning(warnings, "no valid origin") {
		t.Errorf("missing warning: %v", warnings)
	}
}

func TestCORS_WildcardAmongOriginsAllowsAny(t *testing.T) {
	_, _, location, _ := featureSnippets(t, map[string]string{
		"enable-cors": "true", "cors-allow-origin": "https://a.example.com, *",
	})

	if strings.Contains(location, "$http_origin ~*") || !strings.Contains(location, `allow_origin_team_app "*"`) {
		t.Errorf("expected any origin to be allowed:\n%s", location)
	}
}

func TestRateLimit_RequestsAndConnections(t *testing.T) {
	httpCtx, _, location, warnings := featureSnippets(t, map[string]string{
		"limit-rps":              "10",
		"limit-rpm":              "300",
		"limit-connections":      "4",
		"limit-burst-multiplier": "3",
	})

	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	assertContainsAll(t, "http", httpCtx, []string{
		"limit_conn_zone $binary_remote_addr zone=doperator_team_app_conn:5m;",
		"limit_req_zone $binary_remote_addr zone=doperator_team_app_rps:5m rate=10r/s;",
		"limit_req_zone $binary_remote_addr zone=doperator_team_app_rpm:5m rate=300r/m;",
	})
	assertContainsAll(t, "location", location, []string{
		"limit_conn doperator_team_app_conn 4;",
		"limit_req zone=doperator_team_app_rps burst=30 nodelay;",
		"limit_req zone=doperator_team_app_rpm burst=900 nodelay;",
	})
	if strings.Contains(httpCtx, "geo ") {
		t.Errorf("allowlist emitted without limit-whitelist:\n%s", httpCtx)
	}
}

func TestRateLimit_DefaultBurstMultiplier(t *testing.T) {
	_, _, location, _ := featureSnippets(t, map[string]string{"limit-rps": "2"})

	if !strings.Contains(location, "limit_req zone=doperator_team_app_rps burst=10 nodelay;") {
		t.Errorf("expected a burst of 5 times the limit:\n%s", location)
	}
}

func TestRateLimit_Allowlist(t *testing.T) {
	for _, key := range []string{"limit-whitelist", "limit-allowlist"} {
		t.Run(key, func(t *testing.T) {
			httpCtx, _, _, warnings := featureSnippets(t, map[string]string{
				"limit-rps": "5",
				key:         "10.0.0.0/8, 192.168.1.1, 300.1.1.1, 10.0.0.0/33",
			})

			for _, bad := range []string{"300.1.1.1", "10.0.0.0/33"} {
				if !containsWarning(warnings, bad) || strings.Contains(httpCtx, bad) {
					t.Errorf("invalid range %s not dropped with a warning: %v\n%s", bad, warnings, httpCtx)
				}
			}
			assertContainsAll(t, "http", httpCtx, []string{
				"geo $remote_addr $doperator_limit_allowed_team_app {\n    default 0;\n    10.0.0.0/8 1;\n    192.168.1.1 1;\n}",
				"map $doperator_limit_allowed_team_app $doperator_limit_key_team_app {\n" +
					"    0 $binary_remote_addr;\n    1 \"\";\n}",
				"limit_req_zone $doperator_limit_key_team_app zone=doperator_team_app_rps:5m rate=5r/s;",
			})
		})
	}
}

func TestRateLimit_BandwidthAndInvalidValues(t *testing.T) {
	httpCtx, _, location, warnings := featureSnippets(t, map[string]string{
		"limit-rate":        "100",
		"limit-rate-after":  "1024",
		"limit-rps":         "ten",
		"limit-connections": "-1",
	})

	assertContainsAll(t, "location", location, []string{"limit_rate 100k;", "limit_rate_after 1024k;"})
	if strings.Contains(httpCtx+location, "limit_req") || strings.Contains(httpCtx+location, "limit_conn") {
		t.Errorf("invalid limits translated:\n%s\n%s", httpCtx, location)
	}
	for _, key := range []string{"limit-rps", "limit-connections"} {
		if !containsWarning(warnings, key) {
			t.Errorf("missing warning for invalid %s: %v", key, warnings)
		}
	}
}

func TestRateLimit_AllowlistAloneDoesNothing(t *testing.T) {
	httpCtx, _, location, _ := featureSnippets(t, map[string]string{"limit-whitelist": "10.0.0.0/8"})

	if httpCtx != "" || location != "" {
		t.Errorf("allowlist without limits emitted configuration:\n%s\n%s", httpCtx, location)
	}
}

func TestRedirect(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        string
		warning     string
	}{
		{name: "permanent default code",
			annotations: map[string]string{"permanent-redirect": "https://www.example.com"},
			want:        `return 301 "https://www.example.com";`},
		{name: "permanent custom code",
			annotations: map[string]string{"permanent-redirect": "https://example.com/new", "permanent-redirect-code": "308"},
			want:        `return 308 "https://example.com/new";`},
		{name: "permanent invalid code",
			annotations: map[string]string{"permanent-redirect": "https://example.com", "permanent-redirect-code": "200"},
			want:        `return 301 "https://example.com";`, warning: "permanent-redirect-code"},
		{name: "temporal default code",
			annotations: map[string]string{"temporal-redirect": "http://example.com$request_uri"},
			want:        `return 302 "http://example.com$request_uri";`},
		{name: "temporal 308 is out of range",
			annotations: map[string]string{"temporal-redirect": "https://example.com", "temporal-redirect-code": "308"},
			want:        `return 302 "https://example.com";`, warning: "temporal-redirect-code"},
		{name: "temporal wins over permanent",
			annotations: map[string]string{
				"temporal-redirect": "https://temp.example.com", "temporal-redirect-code": "307",
				"permanent-redirect": "https://perm.example.com",
			},
			want: `return 307 "https://temp.example.com";`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, location, warnings := featureSnippets(t, tt.annotations)

			if !strings.Contains(location, tt.want) || strings.Count(location, "return ") != 1 {
				t.Errorf("location does not contain only %q:\n%s", tt.want, location)
			}
			if tt.warning != "" && !containsWarning(warnings, tt.warning) {
				t.Errorf("missing warning about %s: %v", tt.warning, warnings)
			}
			if tt.warning == "" && len(warnings) != 0 {
				t.Errorf("unexpected warnings: %v", warnings)
			}
		})
	}
}

func TestRedirect_RejectsUnsafeURLs(t *testing.T) {
	for _, target := range []string{
		"/relative", "ftp://example.com", "https://example.com/a;b", `https://example.com/"x`,
		"https://example.com/a b", "https://", "javascript:alert(1)",
	} {
		t.Run(target, func(t *testing.T) {
			_, _, location, warnings := featureSnippets(t, map[string]string{"permanent-redirect": target})

			if strings.Contains(location, "return") {
				t.Errorf("unsafe redirect %q translated:\n%s", target, location)
			}
			if !containsWarning(warnings, "permanent-redirect") {
				t.Errorf("no warning for %q", target)
			}
		})
	}
}

func TestRedirect_ComesBeforeForceSSLRedirect(t *testing.T) {
	_, _, location, _ := featureSnippets(t, map[string]string{
		"permanent-redirect": "https://example.com", "force-ssl-redirect": "true",
	})

	if strings.Index(location, `return 301 "https://example.com";`) > strings.Index(location, "return 308") {
		t.Errorf("permanent-redirect must be evaluated before force-ssl-redirect:\n%s", location)
	}
}

func TestAppRoot(t *testing.T) {
	_, server, location, warnings := featureSnippets(t, map[string]string{"app-root": "/app1"})

	if len(warnings) != 0 || location != "" {
		t.Errorf("unexpected warnings %v or location config:\n%s", warnings, location)
	}
	if server != "if ($uri = /) { return 302 $scheme://$http_host/app1; }" {
		t.Errorf("unexpected server snippet:\n%s", server)
	}
	for _, bad := range []string{"app1", "/app;", "/a b", "/$host"} {
		_, server, _, warnings := featureSnippets(t, map[string]string{"app-root": bad})
		if server != "" || !containsWarning(warnings, "app-root") {
			t.Errorf("app-root %q translated (%q) or not warned: %v", bad, server, warnings)
		}
	}
}
