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
	"fmt"
	"regexp"
	"strings"
)

const (
	enableCORSKey           = "enable-cors"
	corsAllowOriginKey      = "cors-allow-origin"
	corsAllowMethodsKey     = "cors-allow-methods"
	corsAllowHeadersKey     = "cors-allow-headers"
	corsAllowCredentialsKey = "cors-allow-credentials"
	corsExposeHeadersKey    = "cors-expose-headers"
	corsMaxAgeKey           = "cors-max-age"

	// ingress-nginx defaults.
	defaultCORSMethods = "GET, PUT, POST, DELETE, PATCH, OPTIONS"
	defaultCORSHeaders = "DNT,Keep-Alive,User-Agent,X-Requested-With,If-Modified-Since,Cache-Control," +
		"Content-Type,Range,Authorization"
	defaultCORSMaxAge = "1728000"
	corsAnyOrigin     = "*"

	annotationTrueValue = "true"
)

// Value syntax accepted by ingress-nginx; anything else falls back to the default.
var (
	corsOriginRe      = regexp.MustCompile(`^([a-z]+://(\*\.)?[A-Za-z0-9\-.]*(:\d+)?|\*|null)$`)
	corsMethodsRe     = regexp.MustCompile(`^([A-Za-z]+,?\s?)+$`)
	corsHeaderListRe  = regexp.MustCompile(`^(([A-Za-z0-9\-_]+|\*),?\s?)+$`)
	corsMaxAgeRe      = regexp.MustCompile(`^[0-9]+$`)
	corsCredentialsRe = regexp.MustCompile(`^(true|false)$`)
)

// corsState holds the CORS annotations of an Ingress.
type corsState struct {
	enabled          bool
	allowOrigin      string
	allowMethods     string
	allowHeaders     string
	allowCredentials string
	exposeHeaders    string
	maxAge           string
}

func applyCORSIngressAnnotationValue(state *nginxIngressSnippetState, suffix, value string) bool {
	switch suffix {
	case enableCORSKey:
		state.cors.enabled = strings.EqualFold(value, annotationTrueValue)
	case corsAllowOriginKey:
		state.cors.allowOrigin = value
	case corsAllowMethodsKey:
		state.cors.allowMethods = value
	case corsAllowHeadersKey:
		state.cors.allowHeaders = value
	case corsAllowCredentialsKey:
		state.cors.allowCredentials = value
	case corsExposeHeadersKey:
		state.cors.exposeHeaders = value
	case corsMaxAgeKey:
		state.cors.maxAge = value
	default:
		return false
	}
	return true
}

// corsHeader is one CORS response header and the NGINX variable that carries its value.
type corsHeader struct {
	name     string
	variable string
	value    string
}

// buildCORSConfig emulates the ingress-nginx CORS support (enable-cors and the cors-* annotations)
// in a location. ingress-nginx sets the headers with more_set_headers, which NGINX Gateway Fabric
// does not have; add_header cannot be conditional, but it omits a header whose value is empty, so
// every header value is an NGINX variable that is only set when the request's Origin is allowed.
// As with ingress-nginx, an allowed preflight (OPTIONS) request is answered with 204 without
// reaching the backend, and the CORS headers replace the ones the backend sends.
func buildCORSConfig(cors corsState, id string) (location, warnings []string) {
	if !cors.enabled {
		return nil, nil
	}
	origins, warnings := corsOrigins(cors.allowOrigin)
	if len(origins) == 0 {
		return nil, append(warnings, fmt.Sprintf("%s%s has no valid origin; CORS is not translated",
			nginxIngressAnnotationPrefix, corsAllowOriginKey))
	}
	headers, headerWarnings := corsHeaders(cors, id)
	warnings = append(warnings, headerWarnings...)

	flag := "$doperator_cors_" + id
	location = []string{fmt.Sprintf("set %s \"\";", flag)}
	matched := []string{fmt.Sprintf("set %s true;", flag)}
	for _, header := range headers {
		location = append(location, fmt.Sprintf("set %s \"\";", header.variable))
		matched = append(matched, fmt.Sprintf("set %s \"%s\";", header.variable, header.value))
	}
	if origins[0] == corsAnyOrigin {
		location = append(location, matched...)
	} else {
		location = append(location, fmt.Sprintf("if ($http_origin ~* \"%s\") {\n%s\n}",
			corsOriginRegex(origins), indentNginxLines(matched)))
	}
	location = append(location,
		fmt.Sprintf("if ($request_method = OPTIONS) { set %s \"${doperator_cors_%s}options\"; }", flag, id),
		fmt.Sprintf("if (%s = trueoptions) { return 204; }", flag),
	)
	for _, header := range headers {
		location = append(location, fmt.Sprintf("proxy_hide_header %s;", header.name))
	}
	for _, header := range headers {
		location = append(location, fmt.Sprintf("add_header %s %s always;", header.name, header.variable))
	}
	return location, warnings
}

// corsOrigins returns the allowed origins, or just "*" when any origin is allowed.
func corsOrigins(raw string) (origins, warnings []string) {
	if strings.TrimSpace(raw) == "" {
		return []string{corsAnyOrigin}, nil
	}
	for _, origin := range parseCommaSeparated(raw) {
		if !corsOriginRe.MatchString(origin) {
			warnings = append(warnings, fmt.Sprintf("%s%s: ignoring invalid origin %q",
				nginxIngressAnnotationPrefix, corsAllowOriginKey, origin))
			continue
		}
		if origin == corsAnyOrigin {
			return []string{corsAnyOrigin}, warnings
		}
		origins = append(origins, origin)
	}
	return origins, warnings
}

// corsOriginRegex matches the Origin header against the allowed origins; "*." in an origin
// stands for one subdomain label, as in ingress-nginx.
func corsOriginRegex(origins []string) string {
	alternatives := make([]string, len(origins))
	for i, origin := range origins {
		alternatives[i] = strings.Replace(regexp.QuoteMeta(origin), `\*`, `[A-Za-z0-9\-]+`, 1)
	}
	return "^(" + strings.Join(alternatives, "|") + ")$"
}

func corsHeaders(cors corsState, id string) ([]corsHeader, []string) {
	var warnings []string
	valueOr := func(key, raw, fallback string, syntax *regexp.Regexp) string {
		if raw == "" {
			return fallback
		}
		if !syntax.MatchString(raw) {
			warnings = append(warnings, fmt.Sprintf("%s%s %q is invalid; using %q",
				nginxIngressAnnotationPrefix, key, raw, fallback))
			return fallback
		}
		return raw
	}
	origin := "$http_origin"
	if allowed, _ := corsOrigins(cors.allowOrigin); len(allowed) > 0 && allowed[0] == corsAnyOrigin {
		origin = corsAnyOrigin
	}
	headers := []corsHeader{{name: "Access-Control-Allow-Origin", value: origin}}
	if valueOr(corsAllowCredentialsKey, cors.allowCredentials, annotationTrueValue, corsCredentialsRe) ==
		annotationTrueValue {
		headers = append(headers, corsHeader{name: "Access-Control-Allow-Credentials", value: annotationTrueValue})
	}
	headers = append(headers,
		corsHeader{name: "Access-Control-Allow-Methods",
			value: valueOr(corsAllowMethodsKey, cors.allowMethods, defaultCORSMethods, corsMethodsRe)},
		corsHeader{name: "Access-Control-Allow-Headers",
			value: valueOr(corsAllowHeadersKey, cors.allowHeaders, defaultCORSHeaders, corsHeaderListRe)},
	)
	if expose := valueOr(corsExposeHeadersKey, cors.exposeHeaders, "", corsHeaderListRe); expose != "" {
		headers = append(headers, corsHeader{name: "Access-Control-Expose-Headers", value: expose})
	}
	headers = append(headers, corsHeader{name: "Access-Control-Max-Age",
		value: valueOr(corsMaxAgeKey, cors.maxAge, defaultCORSMaxAge, corsMaxAgeRe)})
	for i := range headers {
		headers[i].variable = fmt.Sprintf("$doperator_cors_%s_%s", strings.ToLower(
			strings.ReplaceAll(strings.TrimPrefix(headers[i].name, "Access-Control-"), "-", "_")), id)
	}
	return headers, warnings
}
