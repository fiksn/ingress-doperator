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
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	NginxGatewayGroup               = "gateway.nginx.org"
	SnippetsFilterKind              = "SnippetsFilter"
	SnippetsPolicyKind              = "SnippetsPolicy"
	AuthenticationFilterKind        = "AuthenticationFilter"
	RequestHeaderModifierFilterKind = "RequestHeaderModifierFilter"
	RateLimitPolicyKind             = "RateLimitPolicy"
	SnippetsFilterCRDName           = "snippetsfilters.gateway.nginx.org"
	SnippetsPolicyCRDName           = "snippetspolicies.gateway.nginx.org"
	AuthenticationFilterCRDName     = "authenticationfilters.gateway.nginx.org"
	RequestHeaderModifierCRDName    = "requestheadermodifierfilters.gateway.nginx.org"
	RateLimitPolicyCRDName          = "ratelimitpolicies.gateway.nginx.org"
)

const (
	nginxIngressAnnotationPrefix = "nginx.ingress.kubernetes.io/"
	ingressAnnotationPrefix      = "ingress.kubernetes.io/"
	maxK8sNameLength             = 253
	schemeHTTPS                  = "https"
)

const (
	sslRedirectKey           = "ssl-redirect"
	forceSSLRedirectKey      = "force-ssl-redirect"
	preserveTrailingSlashKey = "preserve-trailing-slash"
	configurationSnippetKey  = "configuration-snippet"
	serverSnippetKey         = "server-snippet"
	authSnippetKey           = "auth-snippet"
	proxyBodySizeKey         = "proxy-body-size"
	clientMaxBodySizeKey     = "client-max-body-size"
	proxyRedirectFromKey     = "proxy-redirect-from"
	proxyRedirectToKey       = "proxy-redirect-to"
	proxyBuffersNumberKey    = "proxy-buffers-number"
	browserXssFilterKey      = "browser-xss-filter"
	contentTypeNosniffKey    = "content-type-nosniff"
	referrerPolicyKey        = "referrer-policy"
	sslProxyHeadersKey       = "ssl-proxy-headers"
	allowlistSourceRangeKey  = "allowlist-source-range"
	whitelistSourceRangeKey  = "whitelist-source-range"
	denylistSourceRangeKey   = "denylist-source-range"
	blacklistSourceRangeKey  = "blacklist-source-range"
	customHTTPErrorsKey      = "custom-http-errors"
	fromToWWWRedirectKey     = "from-to-www-redirect"
	rewriteTargetKey         = "rewrite-target"
	useRegexKey              = "use-regex"

	authURLKey                 = "auth-url"
	authSigninKey              = "auth-signin"
	authSigninRedirectParamKey = "auth-signin-redirect-param"
	authMethodKey              = "auth-method"
	authResponseHeadersKey     = "auth-response-headers"
	authRequestRedirectKey     = "auth-request-redirect"
	authCacheKeyKey            = "auth-cache-key"
	authCacheDurationKey       = "auth-cache-duration"
	authKeepaliveKey           = "auth-keepalive"
	authProxySetHeadersKey     = "auth-proxy-set-headers"
)

// AuthInputs carries resolved inputs for external-auth (auth_request) emulation
// that cannot be derived from annotation values alone.
type AuthInputs struct {
	// Identifier is a unique, nginx-safe token per Ingress used to name the
	// generated internal auth location, signin location, upstream and cache zone
	// so that multiple Ingresses sharing a server block never collide.
	Identifier string
	// ProxySetHeaders holds headers resolved from the auth-proxy-set-headers
	// ConfigMap, forwarded to the external auth service on the subrequest.
	ProxySetHeaders map[string]string
}

var nginxIngressDirectiveWhitelist = map[string]struct{}{
	"client-body-buffer-size":     {},
	"http2-push-preload":          {},
	"proxy-buffer-size":           {},
	"proxy-buffering":             {},
	"proxy-busy-buffers-size":     {},
	"proxy-connect-timeout":       {},
	"proxy-cookie-domain":         {},
	"proxy-cookie-path":           {},
	"proxy-http-version":          {},
	"proxy-max-temp-file-size":    {},
	"proxy-next-upstream":         {},
	"proxy-next-upstream-timeout": {},
	"proxy-next-upstream-tries":   {},
	"proxy-read-timeout":          {},
	"proxy-request-buffering":     {},
	"proxy-send-timeout":          {},
	"proxy-ssl-ciphers":           {},
	"proxy-ssl-name":              {},
	"proxy-ssl-protocols":         {},
	"proxy-ssl-server-name":       {},
	"proxy-ssl-verify":            {},
	"proxy-ssl-verify-depth":      {},
	"satisfy":                     {},
	"ssl-ciphers":                 {},
	"ssl-prefer-server-ciphers":   {},
}

func isWhitelistedNginxIngressDirective(suffix string) bool {
	_, ok := nginxIngressDirectiveWhitelist[suffix]
	return ok
}

// nginxServerOnlyDirectives are whitelisted directives that are only valid in the
// nginx server (not location) context. They must be emitted in the http.server
// SnippetsFilter context and therefore collide when multiple Ingresses share a host.
var nginxServerOnlyDirectives = map[string]struct{}{
	"ssl-ciphers":               {},
	"ssl-prefer-server-ciphers": {},
}

func isServerOnlyNginxIngressDirective(suffix string) bool {
	_, ok := nginxServerOnlyDirectives[suffix]
	return ok
}

// ServerOnlySnippetAnnotationKeys returns the full nginx.ingress.kubernetes.io/*
// annotation keys whose directives are server-scoped and therefore cannot be
// deduplicated per-location. Used by the controller to resolve cross-Ingress conflicts.
func ServerOnlySnippetAnnotationKeys() []string {
	keys := make([]string, 0, len(nginxServerOnlyDirectives))
	for suffix := range nginxServerOnlyDirectives {
		keys = append(keys, nginxIngressAnnotationPrefix+suffix)
	}
	sort.Strings(keys)
	return keys
}

// ServerOnlySnippetSuffix returns the directive suffix for a full nginx ingress
// annotation key if that key is server-scoped, plus whether it matched.
func ServerOnlySnippetSuffix(annotationKey string) (string, bool) {
	suffix := strings.TrimPrefix(annotationKey, nginxIngressAnnotationPrefix)
	if suffix == annotationKey {
		return "", false
	}
	if _, ok := nginxServerOnlyDirectives[suffix]; ok {
		return suffix, true
	}
	return "", false
}

func collectSnippetWarnings(annotations map[string]string) []string {
	if annotations == nil {
		return nil
	}
	warnings := make([]string, 0, 2)
	for key := range annotations {
		if !strings.HasPrefix(key, nginxIngressAnnotationPrefix) {
			continue
		}
		suffix := strings.TrimPrefix(key, nginxIngressAnnotationPrefix)
		if suffix == "" {
			continue
		}
		if strings.HasSuffix(suffix, "-snippet") {
			warnings = append(warnings, key)
		}
	}
	sort.Strings(warnings)
	return warnings
}

type ingressAnnotationKey struct {
	fullKey string
	prefix  string
	suffix  string
}

type sslProxyHeader struct {
	headerVar string
	value     string
}

func parseSSLProxyHeaders(raw string) ([]sslProxyHeader, []string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, "||")
	seen := make(map[string]struct{})
	headers := make([]sslProxyHeader, 0, len(parts))
	warnings := make([]string, 0, 1)

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		pieces := strings.SplitN(part, ":", 2)
		if len(pieces) != 2 {
			warnings = append(warnings, "ssl-proxy-headers entry must be HEADER:value")
			continue
		}
		header := strings.ToLower(strings.TrimSpace(pieces[0]))
		value := strings.TrimSpace(pieces[1])
		if header == "" || value == "" {
			warnings = append(warnings, "ssl-proxy-headers entry must be HEADER:value")
			continue
		}
		if containsUnsafeSnippetChars(header) || containsUnsafeSnippetChars(value) {
			warnings = append(warnings, "ssl-proxy-headers entry contains unsafe characters")
			continue
		}
		headerVar := strings.ReplaceAll(header, "-", "_")
		key := headerVar + "\x00" + value
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		headers = append(headers, sslProxyHeader{
			headerVar: headerVar,
			value:     value,
		})
	}

	return headers, warnings
}

func parseCustomHTTPErrors(raw string) ([]string, []string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{})
	out := make([]string, 0, len(parts))
	warnings := make([]string, 0, 1)

	for _, part := range parts {
		code := strings.TrimSpace(part)
		if code == "" {
			continue
		}
		if len(code) != 3 {
			warnings = append(warnings, fmt.Sprintf("custom-http-errors code %q is not a 3-digit HTTP status", code))
			continue
		}
		for _, r := range code {
			if r < '0' || r > '9' {
				warnings = append(warnings, fmt.Sprintf("custom-http-errors code %q is not a 3-digit HTTP status", code))
				code = ""
				break
			}
		}
		if code == "" {
			continue
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, code)
	}

	if len(out) == 0 {
		warnings = append(warnings, "custom-http-errors has no valid HTTP status codes")
	}
	if len(out) > 1 {
		warnings = append(warnings, "custom-http-errors supports multiple codes, but the snippet error handler returns the first code only")
	}

	return out, warnings
}

type crdVersionCacheEntry struct {
	version string
}

var crdVersionCache sync.Map

type IngressClassSnippetsFilter struct {
	Pattern string
	Name    string
}

func ParseIngressClassSnippetsFilters(raw string) ([]IngressClassSnippetsFilter, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	entries := strings.Split(raw, ",")
	out := make([]IngressClassSnippetsFilter, 0, len(entries))
	for _, entry := range entries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid entry %q, expected pattern:name", trimmed)
		}
		pattern := strings.TrimSpace(parts[0])
		name := strings.TrimSpace(parts[1])
		if pattern == "" || name == "" {
			return nil, fmt.Errorf("invalid entry %q, pattern and name must be non-empty", trimmed)
		}
		out = append(out, IngressClassSnippetsFilter{
			Pattern: pattern,
			Name:    name,
		})
	}
	return out, nil
}

type IngressAnnotationSnippetsRule struct {
	AnnotationKey  string
	ValuePattern   string
	SnippetsFilter []string
}

func ParseIngressAnnotationSnippetsRules(raw string) ([]IngressAnnotationSnippetsRule, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	entries := strings.Split(raw, ";")
	out := make([]IngressAnnotationSnippetsRule, 0, len(entries))
	for _, entry := range entries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid entry %q, expected key=value:filter1,filter2", trimmed)
		}
		keyValue := strings.TrimSpace(parts[0])
		filtersRaw := strings.TrimSpace(parts[1])
		kv := strings.SplitN(keyValue, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("invalid entry %q, expected key=value:filter1,filter2", trimmed)
		}
		key := strings.TrimSpace(kv[0])
		valuePattern := strings.TrimSpace(kv[1])
		if key == "" || valuePattern == "" || filtersRaw == "" {
			return nil, fmt.Errorf("invalid entry %q, key, value and filters must be non-empty", trimmed)
		}
		filters := ParseCommaSeparatedAnnotation(map[string]string{"value": filtersRaw}, "value")
		if len(filters) == 0 {
			return nil, fmt.Errorf("invalid entry %q, filters list must be non-empty", trimmed)
		}
		out = append(out, IngressAnnotationSnippetsRule{
			AnnotationKey:  key,
			ValuePattern:   valuePattern,
			SnippetsFilter: filters,
		})
	}
	return out, nil
}

func MatchIngressAnnotationSnippetsRules(
	annotations map[string]string,
	rules []IngressAnnotationSnippetsRule,
) []string {
	if annotations == nil || len(rules) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, rule := range rules {
		if rule.AnnotationKey == "" || rule.ValuePattern == "" {
			continue
		}
		value, ok := annotations[rule.AnnotationKey]
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		matched, err := filepath.Match(rule.ValuePattern, value)
		if err != nil || !matched {
			continue
		}
		for _, name := range rule.SnippetsFilter {
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

// ParseCommaSeparatedAnnotation splits a comma-separated annotation value into unique entries.
func ParseCommaSeparatedAnnotation(annotations map[string]string, key string) []string {
	if annotations == nil {
		return nil
	}
	raw, ok := annotations[key]
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0)
	for _, part := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

// BuildNginxIngressSnippets builds SnippetsFilter entries from NGINX Ingress annotations.
// auth carries inputs for external-auth emulation that cannot be derived from
// annotation values alone (a stable identifier and resolved proxy-set-headers).
// suppress holds server-only directive suffixes (e.g. "ssl-ciphers") that an older
// Ingress on the same host already owns; they are dropped to avoid duplicate directives
// in the shared nginx server block.
// nolint:gocyclo
func BuildNginxIngressSnippets(
	annotations map[string]string,
	auth AuthInputs,
	suppress map[string]struct{},
) ([]map[string]interface{}, []string, bool) {
	if annotations == nil {
		return nil, nil, false
	}

	keys := make([]ingressAnnotationKey, 0, len(annotations))
	for key := range annotations {
		if strings.HasPrefix(key, nginxIngressAnnotationPrefix) {
			suffix := strings.TrimPrefix(key, nginxIngressAnnotationPrefix)
			if suffix != "" {
				keys = append(keys, ingressAnnotationKey{
					fullKey: key,
					prefix:  nginxIngressAnnotationPrefix,
					suffix:  suffix,
				})
			}
			continue
		}
		if strings.HasPrefix(key, ingressAnnotationPrefix) {
			suffix := strings.TrimPrefix(key, ingressAnnotationPrefix)
			if suffix != "" {
				keys = append(keys, ingressAnnotationKey{
					fullKey: key,
					prefix:  ingressAnnotationPrefix,
					suffix:  suffix,
				})
			}
		}
	}
	if len(keys) == 0 {
		return nil, nil, false
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].fullKey < keys[j].fullKey
	})

	state := ingestNginxIngressAnnotations(annotations, keys, suppress)
	lines, warnings := buildNginxDirectiveLines(state)
	authHTTP, authServer, authLocation, authWarnings := buildAuthRequestConfig(state.auth, auth)
	warnings = append(warnings, authWarnings...)
	id := sanitizeNginxIdentifier(auth.Identifier)
	authLines := authSnippetLines{http: authHTTP, server: authServer, location: authLocation}
	snippets := buildNginxSnippetBlocks(state, lines, authLines, id)

	if len(snippets) == 0 {
		return nil, warnings, false
	}

	return snippets, warnings, true
}

// authSnippetLines groups the auth_request configuration lines by nginx context.
type authSnippetLines struct {
	http     []string
	server   []string
	location []string
}

type nginxIngressSnippetState struct {
	lines                 []string
	serverOnlyLines       []string
	sslRedirectOff        bool
	forceSSLRedirect      bool
	proxyBodySizeValue    string
	clientMaxBodySize     string
	proxyRedirectFrom     string
	proxyRedirectTo       string
	proxyBuffersNumber    string
	proxyBufferSize       string
	browserXssFilter      bool
	contentTypeNosniff    bool
	referrerPolicy        string
	sslProxyHeaders       []sslProxyHeader
	whitelistSourceRanges []string
	blacklistSourceRanges []string
	customHTTPErrors      []string
	rewriteTarget         string
	useRegex              bool
	auth                  authState
	warnings              []string
}

type authState struct {
	url                 string
	signin              string
	signinRedirectParam string
	method              string
	responseHeaders     []string
	requestRedirect     string
	cacheKey            string
	cacheDuration       string
	keepalive           string
}

func ingestNginxIngressAnnotations(
	annotations map[string]string,
	keys []ingressAnnotationKey,
	suppress map[string]struct{},
) nginxIngressSnippetState {
	state := nginxIngressSnippetState{
		lines: make([]string, 0, len(keys)),
	}

	for _, entry := range keys {
		raw := annotations[entry.fullKey]
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if entry.prefix == nginxIngressAnnotationPrefix {
			if handled := applyIngressAnnotationValue(&state, entry.suffix, value); handled {
				continue
			}

			if !isWhitelistedNginxIngressDirective(entry.suffix) {
				continue
			}
			if !isSafeSnippetValue(&state, entry.fullKey, value) {
				continue
			}
			directive := strings.ReplaceAll(entry.suffix, "-", "_")
			if isServerOnlyNginxIngressDirective(entry.suffix) {
				if _, dropped := suppress[entry.suffix]; dropped {
					continue
				}
				state.serverOnlyLines = append(state.serverOnlyLines, fmt.Sprintf("%s %s;", directive, value))
				continue
			}
			if entry.suffix == "proxy-buffer-size" {
				state.proxyBufferSize = value
			}
			state.lines = append(state.lines, fmt.Sprintf("%s %s;", directive, value))
			continue
		}
		if entry.prefix == ingressAnnotationPrefix {
			applyLegacyIngressAnnotationValue(&state, entry.suffix, value)
		}
	}

	return state
}

func applyIngressAnnotationValue(state *nginxIngressSnippetState, suffix, value string) bool {
	if applyAuthIngressAnnotationValue(state, suffix, value) {
		return true
	}

	parsedRanges := func(raw string) []string {
		return ParseCommaSeparatedAnnotation(map[string]string{"value": raw}, "value")
	}

	switch suffix {
	case configurationSnippetKey, serverSnippetKey, authSnippetKey:
		return true
	case proxyBodySizeKey:
		if !isSafeSnippetValue(state, suffix, value) {
			return true
		}
		state.proxyBodySizeValue = value
		return true
	case allowlistSourceRangeKey, whitelistSourceRangeKey:
		ranges := parsedRanges(value)
		ranges = filterSafeSnippetValues(state, suffix, ranges)
		state.whitelistSourceRanges = append(state.whitelistSourceRanges, ranges...)
		return true
	case denylistSourceRangeKey, blacklistSourceRangeKey:
		ranges := parsedRanges(value)
		ranges = filterSafeSnippetValues(state, suffix, ranges)
		state.blacklistSourceRanges = append(state.blacklistSourceRanges, ranges...)
		return true
	case clientMaxBodySizeKey:
		if !isSafeSnippetValue(state, suffix, value) {
			return true
		}
		state.clientMaxBodySize = value
		return true
	case proxyRedirectFromKey:
		if !isSafeSnippetValue(state, suffix, value) {
			return true
		}
		state.proxyRedirectFrom = value
		return true
	case proxyRedirectToKey:
		if !isSafeSnippetValue(state, suffix, value) {
			return true
		}
		state.proxyRedirectTo = value
		return true
	case proxyBuffersNumberKey:
		if !isSafeSnippetValue(state, suffix, value) {
			return true
		}
		state.proxyBuffersNumber = value
		return true
	case sslRedirectKey:
		if strings.EqualFold(value, "false") {
			state.sslRedirectOff = true
		}
		return true
	case forceSSLRedirectKey:
		if strings.EqualFold(value, "true") {
			state.forceSSLRedirect = true
		}
		return true
	case preserveTrailingSlashKey:
		// preserve-trailing-slash has no SnippetsFilter equivalent; recognized and ignored.
		return true
	case customHTTPErrorsKey:
		codes, warnings := parseCustomHTTPErrors(value)
		if len(codes) > 0 {
			state.customHTTPErrors = codes
		}
		if len(warnings) > 0 {
			state.warnings = append(state.warnings, warnings...)
		}
		return true
	case fromToWWWRedirectKey:
		if strings.EqualFold(value, "true") {
			state.warnings = append(state.warnings, "from-to-www-redirect is not supported in snippets; missing host information, annotation ignored")
		}
		return true
	case rewriteTargetKey:
		if !isSafeSnippetValue(state, suffix, value) {
			return true
		}
		state.rewriteTarget = value
		return true
	case useRegexKey:
		if strings.EqualFold(value, "true") {
			state.useRegex = true
		}
		return true
	default:
		return false
	}
}

// applyAuthIngressAnnotationValue records external-auth (auth_request) inputs.
// Returns true when suffix is an auth-* annotation it consumes.
func applyAuthIngressAnnotationValue(state *nginxIngressSnippetState, suffix, value string) bool {
	switch suffix {
	case authURLKey:
		state.auth.url = value
	case authSigninKey:
		state.auth.signin = value
	case authSigninRedirectParamKey:
		state.auth.signinRedirectParam = value
	case authMethodKey:
		state.auth.method = value
	case authResponseHeadersKey:
		state.auth.responseHeaders = ParseCommaSeparatedAnnotation(map[string]string{"value": value}, "value")
	case authRequestRedirectKey:
		state.auth.requestRedirect = value
	case authCacheKeyKey:
		state.auth.cacheKey = value
	case authCacheDurationKey:
		state.auth.cacheDuration = value
	case authKeepaliveKey:
		state.auth.keepalive = value
	case authProxySetHeadersKey:
		// Resolved by the controller into AuthInputs.ProxySetHeaders; the raw
		// ConfigMap reference value is consumed here only to mark it handled.
	default:
		return false
	}
	return true
}

func applyLegacyIngressAnnotationValue(state *nginxIngressSnippetState, suffix, value string) bool {
	switch suffix {
	case browserXssFilterKey:
		if strings.EqualFold(value, "true") {
			state.browserXssFilter = true
		}
		return true
	case contentTypeNosniffKey:
		if strings.EqualFold(value, "true") {
			state.contentTypeNosniff = true
		}
		return true
	case referrerPolicyKey:
		if !isSafeSnippetValue(state, suffix, value) {
			return true
		}
		state.referrerPolicy = value
		return true
	case sslProxyHeadersKey:
		headers, warnings := parseSSLProxyHeaders(value)
		if len(headers) > 0 {
			state.sslProxyHeaders = headers
		}
		if len(warnings) > 0 {
			state.warnings = append(state.warnings, warnings...)
		}
		return true
	case forceSSLRedirectKey:
		if strings.EqualFold(value, "true") {
			state.forceSSLRedirect = true
		}
		return true
	default:
		return false
	}
}

func buildNginxDirectiveLines(state nginxIngressSnippetState) ([]string, []string) {
	lines := append([]string{}, state.lines...)
	warnings := append([]string{}, state.warnings...)

	if strings.TrimSpace(state.rewriteTarget) != "" && strings.Contains(state.rewriteTarget, "$") && !state.useRegex {
		warnings = append(warnings, "rewrite-target contains capture references but use-regex is not enabled")
	}

	if state.clientMaxBodySize != "" {
		lines = append(lines, fmt.Sprintf("client_max_body_size %s;", state.clientMaxBodySize))
	} else if state.proxyBodySizeValue != "" {
		lines = append(lines, fmt.Sprintf("client_max_body_size %s;", state.proxyBodySizeValue))
	}

	if state.proxyRedirectFrom != "" {
		lowerFrom := strings.ToLower(state.proxyRedirectFrom)
		if lowerFrom == "off" || lowerFrom == "default" {
			lines = append(lines, fmt.Sprintf("proxy_redirect %s;", state.proxyRedirectFrom))
		} else if state.proxyRedirectTo != "" {
			lines = append(lines, fmt.Sprintf("proxy_redirect %s %s;", state.proxyRedirectFrom, state.proxyRedirectTo))
		} else {
			warnings = append(warnings, "proxy-redirect-from requires proxy-redirect-to (or set proxy-redirect-from to off/default)")
		}
	} else if state.proxyRedirectTo != "" {
		warnings = append(warnings, "proxy-redirect-to requires proxy-redirect-from")
	}

	if state.proxyBuffersNumber != "" {
		if state.proxyBufferSize == "" {
			warnings = append(warnings, "proxy-buffers-number requires proxy-buffer-size to compute proxy_buffers")
		} else {
			lines = append(lines, fmt.Sprintf("proxy_buffers %s %s;", state.proxyBuffersNumber, state.proxyBufferSize))
		}
	}

	return lines, warnings
}

// buildNginxSnippetBlocks partitions the generated directives by nginx context.
// Only genuinely server-scoped directives (ssl_ciphers, per-Ingress named error
// locations, auth server blocks) go into http.server; everything else that is valid
// in a location goes into http.server.location so multiple Ingresses sharing a host
// never emit duplicate directives into the same server block.
func buildNginxSnippetBlocks(
	state nginxIngressSnippetState,
	lines []string,
	auth authSnippetLines,
	id string,
) []map[string]interface{} {
	snippets := make([]map[string]interface{}, 0, 3)
	snippets = appendSnippet(snippets, "http", buildHTTPContextLines(state, auth.http))
	snippets = appendSnippet(snippets, "http.server", buildServerContextLines(state, auth.server, id))
	snippets = appendSnippet(snippets, "http.server.location",
		buildLocationContextLines(state, lines, auth.location, id))
	return snippets
}

func appendSnippet(snippets []map[string]interface{}, nginxContext string, lines []string) []map[string]interface{} {
	if len(lines) == 0 {
		return snippets
	}
	return append(snippets, map[string]interface{}{
		"context": nginxContext,
		"value":   strings.Join(lines, "\n"),
	})
}

func buildHTTPContextLines(state nginxIngressSnippetState, authHTTP []string) []string {
	httpLines := make([]string, 0, len(authHTTP)+1)
	if state.sslRedirectOff {
		httpLines = append(httpLines, "ssl_redirect off;")
	}
	return append(httpLines, authHTTP...)
}

func buildServerContextLines(state nginxIngressSnippetState, authServer []string, id string) []string {
	serverLines := append([]string{}, state.serverOnlyLines...)
	if len(state.customHTTPErrors) > 0 {
		serverLines = append(serverLines,
			fmt.Sprintf("location @ingress_doperator_error_%s {\n    return %s;\n}", id, state.customHTTPErrors[0]))
	}
	return append(serverLines, authServer...)
}

func buildLocationContextLines(state nginxIngressSnippetState, lines, authLocation []string, id string) []string {
	out := append([]string{}, lines...)
	if len(state.customHTTPErrors) > 0 {
		out = append(out,
			"proxy_intercept_errors on;",
			fmt.Sprintf("error_page %s = @ingress_doperator_error_%s;",
				strings.Join(state.customHTTPErrors, " "), id),
		)
	}
	out = appendSecurityHeaderLines(out, state)
	out = appendForceSSLRedirectLines(out, state)
	out = appendRewriteAndACLLines(out, state)
	return append(out, authLocation...)
}

func appendSecurityHeaderLines(lines []string, state nginxIngressSnippetState) []string {
	if state.browserXssFilter {
		lines = append(lines, "add_header X-XSS-Protection \"1; mode=block\" always;")
	}
	if state.contentTypeNosniff {
		lines = append(lines, "add_header X-Content-Type-Options \"nosniff\" always;")
	}
	if strings.TrimSpace(state.referrerPolicy) != "" {
		lines = append(lines,
			fmt.Sprintf("add_header Referrer-Policy %q always;", escapeHeaderValue(state.referrerPolicy)))
	}
	return lines
}

func appendForceSSLRedirectLines(lines []string, state nginxIngressSnippetState) []string {
	if !state.forceSSLRedirect {
		return lines
	}
	lines = append(lines,
		"set $ingress_doperator_needs_redirect 0;",
		"if ($scheme != \"https\") { set $ingress_doperator_needs_redirect 1; }",
	)
	for _, header := range state.sslProxyHeaders {
		lines = append(lines, fmt.Sprintf(
			"if ($http_%s = %q) { set $ingress_doperator_needs_redirect 0; }",
			header.headerVar, header.value))
	}
	return append(lines,
		"add_header Strict-Transport-Security \"max-age=31536000\" always;",
		"if ($ingress_doperator_needs_redirect = 1) { return 308 https://$server_name$request_uri; }",
	)
}

func appendRewriteAndACLLines(lines []string, state nginxIngressSnippetState) []string {
	if strings.TrimSpace(state.rewriteTarget) != "" {
		target := strings.TrimSpace(state.rewriteTarget)
		pattern := "^"
		if state.useRegex || strings.Contains(target, "$") {
			pattern = "^(.*)$"
		}
		lines = append(lines, fmt.Sprintf("rewrite %s %s break;", pattern, target))
	}
	for _, cidr := range uniqueStrings(state.blacklistSourceRanges) {
		lines = append(lines, fmt.Sprintf("deny %s;", cidr))
	}
	for _, cidr := range uniqueStrings(state.whitelistSourceRanges) {
		lines = append(lines, fmt.Sprintf("allow %s;", cidr))
	}
	if len(state.whitelistSourceRanges) > 0 {
		lines = append(lines, "deny all;")
	}
	return lines
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

func escapeHeaderValue(value string) string {
	return strings.ReplaceAll(value, "\"", "\\\"")
}

// sanitizeQuotedRegex escapes backslashes and double quotes in a location path
// so paths cannot escape NGINX configuration.
//
//nolint:unused // Reserved for future use in rewrite path sanitization.
func sanitizeQuotedRegex(path string) string {
	builder := strings.Builder{}
	builder.Grow(2 * len(path))
	// note that iterating over a string iterates over its runes, not bytes
	for _, r := range path {
		if r == '\\' || r == '"' {
			builder.WriteByte('\\')
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func containsUnsafeSnippetChars(value string) bool {
	return strings.ContainsAny(value, "\r\n;{}")
}

func isSafeSnippetValue(state *nginxIngressSnippetState, key, value string) bool {
	if containsUnsafeSnippetChars(value) {
		state.warnings = append(state.warnings,
			fmt.Sprintf("annotation %s contains unsafe characters (\\n, \\r, ';', '{', '}'); ignoring", key))
		return false
	}
	return true
}

func filterSafeSnippetValues(state *nginxIngressSnippetState, key string, values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if containsUnsafeSnippetChars(value) {
			state.warnings = append(state.warnings,
				fmt.Sprintf("annotation %s contains unsafe characters (\\n, \\r, ';', '{', '}'); ignoring value %q", key, value))
			continue
		}
		out = append(out, value)
	}
	return out
}

var (
	authHeaderNameRe = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	authTokenRe      = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	authNumberRe     = regexp.MustCompile(`^[0-9]+$`)
	authCacheValidRe = regexp.MustCompile(`^[0-9smhd ]+$`)
	authHTTPMethods  = map[string]struct{}{
		"GET": {}, "HEAD": {}, "POST": {}, "PUT": {},
		"DELETE": {}, "PATCH": {}, "OPTIONS": {},
	}
)

// AuthIdentifier returns the stable per-Ingress token used to seed AuthInputs.Identifier.
// The webhook and controller MUST agree on this value so they generate identical
// NGINX object names and never overwrite each other's SnippetsFilter.
func AuthIdentifier(namespace, name string) string {
	return fmt.Sprintf("%s-%s", namespace, name)
}

// buildAuthRequestConfig emulates the ingress-nginx external-auth (auth_request)
// flow as NGINX directives, partitioned by SnippetsFilter context. It returns
// http-, server- and location-context line groups plus warnings for invalid input.
func buildAuthRequestConfig(auth authState, inputs AuthInputs) (httpLines, serverLines, locationLines, warnings []string) {
	rawURL := strings.TrimSpace(auth.url)
	if rawURL == "" {
		return nil, nil, nil, nil
	}
	parsed, ok := validateAuthURL(rawURL)
	if !ok {
		return nil, nil, nil, []string{
			fmt.Sprintf("auth-url %q is not a valid http(s) URL; ignoring external auth", rawURL),
		}
	}

	id := sanitizeNginxIdentifier(inputs.Identifier)
	locationLines = append(locationLines, fmt.Sprintf("auth_request /_doperator_auth_%s;", id))

	respLines, respWarnings := buildAuthResponseHeaderLines(auth.responseHeaders)
	locationLines = append(locationLines, respLines...)
	warnings = append(warnings, respWarnings...)

	signinLine, signinBlock, signinWarnings := buildAuthSignin(auth, id)
	warnings = append(warnings, signinWarnings...)
	if signinLine != "" {
		locationLines = append(locationLines, signinLine)
		serverLines = append(serverLines, signinBlock)
	}

	authBlock, blockHTTP, blockWarnings := buildAuthLocationBlock(auth, inputs, parsed, id)
	warnings = append(warnings, blockWarnings...)
	httpLines = append(httpLines, blockHTTP...)
	serverLines = append([]string{authBlock}, serverLines...)

	return httpLines, serverLines, locationLines, warnings
}

func buildAuthResponseHeaderLines(headers []string) (lines, warnings []string) {
	for i, hdr := range headers {
		if !authHeaderNameRe.MatchString(hdr) {
			warnings = append(warnings,
				fmt.Sprintf("auth-response-headers entry %q is not a valid header name; ignoring", hdr))
			continue
		}
		varName := fmt.Sprintf("$doperator_auth_h%d", i)
		upstreamVar := "$upstream_http_" + strings.ReplaceAll(strings.ToLower(hdr), "-", "_")
		lines = append(lines,
			fmt.Sprintf("auth_request_set %s %s;", varName, upstreamVar),
			fmt.Sprintf("proxy_set_header %s %s;", hdr, varName),
		)
	}
	return lines, warnings
}

func buildAuthSignin(auth authState, id string) (locationLine, serverBlock string, warnings []string) {
	signin := strings.TrimSpace(auth.signin)
	if signin == "" {
		return "", "", nil
	}
	if _, ok := validateAuthURL(signin); !ok {
		return "", "", []string{fmt.Sprintf("auth-signin %q is not a valid http(s) URL; ignoring", signin)}
	}
	signin = strings.ReplaceAll(signin, "$escaped_request_uri", "$request_uri")
	if param := strings.TrimSpace(auth.signinRedirectParam); param != "" {
		if authTokenRe.MatchString(param) {
			if !strings.Contains(signin, "?") {
				signin = fmt.Sprintf("%s?%s=$request_uri", signin, param)
			}
		} else {
			warnings = append(warnings,
				fmt.Sprintf("auth-signin-redirect-param %q is not a valid query parameter name; ignoring", param))
		}
	}
	signinLoc := "@doperator_signin_" + id
	locationLine = fmt.Sprintf("error_page 401 = %s;", signinLoc)
	serverBlock = fmt.Sprintf("location %s {\n    internal;\n    return 302 %s;\n}", signinLoc, signin)
	return locationLine, serverBlock, warnings
}

func buildAuthLocationBlock(
	auth authState, inputs AuthInputs, parsed *url.URL, id string,
) (block string, httpLines, warnings []string) {
	lines := []string{
		"internal;",
		"proxy_pass_request_body off;",
		"proxy_set_header Content-Length \"\";",
		"proxy_set_header X-Original-URL $scheme://$http_host$request_uri;",
		"proxy_set_header X-Original-URI $request_uri;",
		"proxy_set_header X-Original-Method $request_method;",
		"proxy_set_header X-Scheme $scheme;",
		"proxy_set_header X-Sender-Host $host;",
	}

	requestRedirect := strings.TrimSpace(auth.requestRedirect)
	if requestRedirect == "" {
		requestRedirect = "$request_uri"
	} else if containsUnsafeSnippetChars(requestRedirect) {
		warnings = append(warnings, "auth-request-redirect contains unsafe characters; using $request_uri")
		requestRedirect = "$request_uri"
	}
	lines = append(lines, fmt.Sprintf("proxy_set_header X-Auth-Request-Redirect %s;", requestRedirect))

	if method := strings.ToUpper(strings.TrimSpace(auth.method)); method != "" {
		if _, ok := authHTTPMethods[method]; ok {
			lines = append(lines, fmt.Sprintf("proxy_method %s;", method))
		} else {
			warnings = append(warnings,
				fmt.Sprintf("auth-method %q is not a recognized HTTP method; ignoring", method))
		}
	}

	headerLines, headerWarnings := buildAuthProxySetHeaderLines(inputs.ProxySetHeaders)
	lines = append(lines, headerLines...)
	warnings = append(warnings, headerWarnings...)

	lines = append(lines, fmt.Sprintf("proxy_set_header Host %s;", parsed.Host))
	if parsed.Scheme == schemeHTTPS {
		lines = append(lines, "proxy_ssl_server_name on;", fmt.Sprintf("proxy_ssl_name %s;", parsed.Hostname()))
	}

	proxyPass, keepaliveHTTP, keepaliveWarnings := buildAuthProxyPass(auth, parsed, id)
	warnings = append(warnings, keepaliveWarnings...)
	httpLines = append(httpLines, keepaliveHTTP...)
	if len(keepaliveHTTP) > 0 {
		lines = append(lines, "proxy_http_version 1.1;", "proxy_set_header Connection \"\";")
	}

	cacheLines, cacheHTTP, cacheWarnings := buildAuthCache(auth, id)
	warnings = append(warnings, cacheWarnings...)
	httpLines = append(httpLines, cacheHTTP...)
	lines = append(lines, cacheLines...)

	lines = append(lines, proxyPass)
	block = fmt.Sprintf("location = /_doperator_auth_%s {\n%s\n}", id, indentNginxLines(lines))
	return block, httpLines, warnings
}

func buildAuthProxySetHeaderLines(headers map[string]string) (lines, warnings []string) {
	for _, name := range sortedMapKeys(headers) {
		value := headers[name]
		if !authHeaderNameRe.MatchString(name) {
			warnings = append(warnings,
				fmt.Sprintf("auth-proxy-set-headers key %q is not a valid header name; ignoring", name))
			continue
		}
		if containsUnsafeSnippetChars(value) {
			warnings = append(warnings,
				fmt.Sprintf("auth-proxy-set-headers value for %q contains unsafe characters; ignoring", name))
			continue
		}
		lines = append(lines, fmt.Sprintf("proxy_set_header %s \"%s\";", name, escapeHeaderValue(value)))
	}
	return lines, warnings
}

func buildAuthProxyPass(auth authState, parsed *url.URL, id string) (proxyPass string, httpLines, warnings []string) {
	keepalive := strings.TrimSpace(auth.keepalive)
	if keepalive == "" {
		return fmt.Sprintf("proxy_pass %s;", strings.TrimSpace(auth.url)), nil, nil
	}
	if !authNumberRe.MatchString(keepalive) {
		warnings = append(warnings,
			fmt.Sprintf("auth-keepalive %q is not a positive integer; disabling keepalive", keepalive))
		return fmt.Sprintf("proxy_pass %s;", strings.TrimSpace(auth.url)), nil, warnings
	}
	upstreamName := "doperator_auth_" + id
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == schemeHTTPS {
			port = "443"
		} else {
			port = "80"
		}
	}
	httpLines = append(httpLines, fmt.Sprintf(
		"upstream %s {\n    server %s:%s;\n    keepalive %s;\n}", upstreamName, parsed.Hostname(), port, keepalive))
	proxyPass = fmt.Sprintf("proxy_pass %s://%s%s;", parsed.Scheme, upstreamName, parsed.RequestURI())
	return proxyPass, httpLines, warnings
}

func buildAuthCache(auth authState, id string) (locationLines, httpLines, warnings []string) {
	cacheKey := strings.TrimSpace(auth.cacheKey)
	if cacheKey == "" {
		return nil, nil, nil
	}
	if containsUnsafeSnippetChars(cacheKey) {
		return nil, nil, []string{"auth-cache-key contains unsafe characters; disabling auth caching"}
	}
	zone := "doperator_auth_" + id
	httpLines = append(httpLines, fmt.Sprintf(
		"proxy_cache_path /var/cache/nginx/%s levels=1:2 keys_zone=%s:1m max_size=10m inactive=60m use_temp_path=off;",
		zone, zone))
	locationLines = append(locationLines,
		fmt.Sprintf("proxy_cache %s;", zone),
		fmt.Sprintf("proxy_cache_key \"%s\";", escapeHeaderValue(cacheKey)),
	)
	if duration := strings.TrimSpace(auth.cacheDuration); duration != "" {
		if authCacheValidRe.MatchString(duration) {
			locationLines = append(locationLines, fmt.Sprintf("proxy_cache_valid %s;", duration))
		} else {
			warnings = append(warnings,
				fmt.Sprintf("auth-cache-duration %q is not a valid duration; ignoring", duration))
		}
	}
	return locationLines, httpLines, warnings
}

// sanitizeNginxIdentifier reduces an arbitrary string to lowercase [a-z0-9_] so
// it can safely name an NGINX location, upstream or cache zone.
func sanitizeNginxIdentifier(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "default"
	}
	return out
}

func indentNginxLines(lines []string) string {
	indented := make([]string, len(lines))
	for i, line := range lines {
		indented[i] = "    " + line
	}
	return strings.Join(indented, "\n")
}

func validateAuthURL(raw string) (*url.URL, bool) {
	if raw == "" || containsUnsafeSnippetChars(raw) {
		return nil, false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, false
	}
	if parsed.Scheme != "http" && parsed.Scheme != schemeHTTPS {
		return nil, false
	}
	if parsed.Host == "" {
		return nil, false
	}
	return parsed, true
}

func sortedMapKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// NginxIngressSnippetWarningAnnotations returns full annotation keys that should emit warnings when ignored.
func NginxIngressSnippetWarningAnnotations(annotations map[string]string) []string {
	return collectSnippetWarnings(annotations)
}

// AutomaticSnippetsFilterName returns a stable name for annotation-based SnippetsFilter resources.
func AutomaticSnippetsFilterName(ingressName string) string {
	base := fmt.Sprintf("automatic-%s-annotations", ingressName)
	if len(base) <= maxK8sNameLength {
		return base
	}
	trimmed := base[:maxK8sNameLength]
	return strings.TrimRight(trimmed, "-")
}

// EnsureSnippetsFilterForIngress creates or updates a SnippetsFilter for the given Ingress.
// Returns true if the resource exists and can be referenced safely.
func EnsureSnippetsFilterForIngress(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	httpRoute *gatewayv1.HTTPRoute,
	owner client.Object,
	ingressNamespace string,
	ingressName string,
	filterName string,
	snippets []map[string]interface{},
) (bool, error) {
	logger := log.FromContext(ctx)
	if httpRoute == nil {
		return false, nil
	}
	if len(snippets) == 0 {
		return false, nil
	}
	crdName, ok := snippetsCRDNameForKind(SnippetsFilterKind)
	if !ok {
		return false, nil
	}
	version, ok, err := getCRDVersion(ctx, c, crdName)
	if err != nil || !ok {
		return false, err
	}

	gvk := schema.GroupVersionKind{Group: NginxGatewayGroup, Version: version, Kind: SnippetsFilterKind}
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(gvk)
	desired.SetName(filterName)
	desired.SetNamespace(httpRoute.Namespace)

	if scheme != nil && owner != nil {
		if err := controllerutil.SetControllerReference(owner, desired, scheme); err != nil {
			return false, err
		}
	}

	annotations := desired.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[ManagedByAnnotation] = ManagedByValue
	annotations[SourceAnnotation] = fmt.Sprintf("%s/%s", ingressNamespace, ingressName)
	desired.SetAnnotations(annotations)

	desired.Object["spec"] = map[string]interface{}{
		"snippets": snippets,
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(gvk)
	err = c.Get(ctx, types.NamespacedName{Namespace: httpRoute.Namespace, Name: filterName}, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Creating SnippetsFilter", "namespace", httpRoute.Namespace, "name", filterName)
			if err := c.Create(ctx, desired); err != nil {
				return false, fmt.Errorf("failed to create SnippetsFilter %s/%s: %w", httpRoute.Namespace, filterName, err)
			}
			return true, nil
		}
		return false, err
	}

	if !IsManagedByUs(existing) {
		logger.Info("SnippetsFilter exists but is not managed by us, skipping",
			"namespace", httpRoute.Namespace,
			"name", filterName)
		return false, nil
	}

	existing.SetAnnotations(desired.GetAnnotations())
	existing.SetOwnerReferences(desired.GetOwnerReferences())
	existing.Object["spec"] = desired.Object["spec"]
	logger.Info("Updating SnippetsFilter", "namespace", httpRoute.Namespace, "name", filterName)
	if err := c.Update(ctx, existing); err != nil {
		return false, fmt.Errorf("failed to update SnippetsFilter %s/%s: %w", httpRoute.Namespace, filterName, err)
	}
	return true, nil
}

// EnsureSnippetsFilterCopyForHTTPRoute copies a SnippetsFilter from source namespace into the HTTPRoute namespace.
// Returns true if the destination resource was created/updated and can be referenced safely.
func EnsureSnippetsFilterCopyForHTTPRoute(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	sourceNamespace string,
	destNamespace string,
	ingressNamespace string,
	ingressName string,
	filterName string,
	ownerHTTPRouteName string,
) (bool, error) {
	logger := log.FromContext(ctx)
	crdName, ok := snippetsCRDNameForKind(SnippetsFilterKind)
	if !ok {
		return false, nil
	}
	version, ok, err := getCRDVersion(ctx, c, crdName)
	if err != nil || !ok {
		return false, err
	}

	gvk := schema.GroupVersionKind{Group: NginxGatewayGroup, Version: version, Kind: SnippetsFilterKind}
	source := &unstructured.Unstructured{}
	source.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, types.NamespacedName{Namespace: sourceNamespace, Name: filterName}, source); err != nil {
		if apierrors.IsNotFound(err) {
			if destNamespace == "" {
				return false, nil
			}
			existing := &unstructured.Unstructured{}
			existing.SetGroupVersionKind(gvk)
			if err := c.Get(ctx, types.NamespacedName{Namespace: destNamespace, Name: filterName}, existing); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			if !IsManagedByUs(existing) {
				return false, nil
			}
			logger.Info("Deleting SnippetsFilter copy (source missing)",
				"namespace", destNamespace,
				"name", filterName)
			if err := c.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("failed to delete SnippetsFilter %s/%s: %w", destNamespace, filterName, err)
			}
			return false, nil
		}
		return false, err
	}

	if sourceNamespace == destNamespace {
		return true, nil
	}

	spec := map[string]interface{}{}
	if rawSpec, ok := source.Object["spec"]; ok {
		if specMap, ok := rawSpec.(map[string]interface{}); ok && specMap != nil {
			copiedSpec := runtime.DeepCopyJSON(specMap)
			if copiedSpec != nil {
				spec = copiedSpec
			}
		}
	}

	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(gvk)
	desired.SetName(filterName)
	desired.SetNamespace(destNamespace)
	desired.SetLabels(copyStringMap(source.GetLabels()))
	annotations := copyStringMap(source.GetAnnotations())
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[ManagedByAnnotation] = ManagedByValue
	annotations[SourceAnnotation] = fmt.Sprintf("%s/%s", ingressNamespace, ingressName)
	desired.SetAnnotations(annotations)
	desired.Object["spec"] = spec

	// Intentionally do not set an ownerRef on copied SnippetsFilters.
	// These copies can be shared across multiple Ingresses in the same namespace.
	// Setting a single ownerRef causes GC to delete the shared copy when that one
	// HTTPRoute is deleted or recreated.

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(gvk)
	err = c.Get(ctx, types.NamespacedName{Namespace: destNamespace, Name: filterName}, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Creating SnippetsFilter copy",
				"namespace", destNamespace,
				"name", filterName)
			if err := c.Create(ctx, desired); err != nil {
				return false, fmt.Errorf("failed to create SnippetsFilter %s/%s: %w", destNamespace, filterName, err)
			}
			return true, nil
		}
		return false, err
	}

	if !IsManagedByUs(existing) {
		logger.Info("SnippetsFilter exists but is not managed by us, skipping copy",
			"namespace", destNamespace,
			"name", filterName)
		return false, nil
	}

	existing.SetLabels(desired.GetLabels())
	existing.SetAnnotations(desired.GetAnnotations())
	existing.SetOwnerReferences(desired.GetOwnerReferences())
	existing.Object["spec"] = spec
	logger.Info("Updating SnippetsFilter copy",
		"namespace", destNamespace,
		"name", filterName)
	if err := c.Update(ctx, existing); err != nil {
		return false, fmt.Errorf("failed to update SnippetsFilter %s/%s: %w", destNamespace, filterName, err)
	}
	return true, nil
}

// ValidateSnippetsFilterExists ensures a SnippetsFilter exists in the given namespace.
func ValidateSnippetsFilterExists(ctx context.Context, c client.Reader, namespace, name string) error {
	crdName, ok := snippetsCRDNameForKind(SnippetsFilterKind)
	if !ok {
		return nil
	}
	version, ok, err := getCRDVersion(ctx, c, crdName)
	if err != nil || !ok {
		return err
	}
	gvk := schema.GroupVersionKind{Group: NginxGatewayGroup, Version: version, Kind: SnippetsFilterKind}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("SnippetsFilter %s/%s not found", namespace, name)
		}
		return err
	}
	return nil
}

// EnsureExtensionResource copies an ExtensionRef resource from sourceNamespace into destNamespace.
// Returns true if the destination resource was created/updated and can be referenced safely.
func EnsureExtensionResource(
	ctx context.Context,
	c client.Client,
	kind string,
	name string,
	sourceNamespace string,
	destNamespace string,
	ingressNamespace string,
	ingressName string,
	targetRef map[string]interface{},
) (bool, error) {
	logger := log.FromContext(ctx)
	crdName, ok := extensionCRDNameForKind(kind)
	if !ok {
		return false, nil
	}
	version, ok, err := getCRDVersion(ctx, c, crdName)
	if err != nil || !ok {
		return false, err
	}

	gvk := schema.GroupVersionKind{Group: NginxGatewayGroup, Version: version, Kind: kind}
	source := &unstructured.Unstructured{}
	source.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, types.NamespacedName{Namespace: sourceNamespace, Name: name}, source); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	if sourceNamespace == destNamespace {
		return true, nil
	}

	spec := map[string]interface{}{}
	if rawSpec, ok := source.Object["spec"]; ok {
		if specMap, ok := rawSpec.(map[string]interface{}); ok && specMap != nil {
			copiedSpec := runtime.DeepCopyJSON(specMap)
			if copiedSpec != nil {
				spec = copiedSpec
			}
		}
	}
	if targetRef != nil {
		if spec == nil {
			spec = map[string]interface{}{}
		}
		spec["targetRef"] = targetRef
	}

	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(gvk)
	desired.SetName(name)
	desired.SetNamespace(destNamespace)
	desired.SetLabels(copyStringMap(source.GetLabels()))
	annotations := copyStringMap(source.GetAnnotations())
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[ManagedByAnnotation] = ManagedByValue
	annotations[SourceAnnotation] = fmt.Sprintf("%s/%s", ingressNamespace, ingressName)
	desired.SetAnnotations(annotations)
	desired.Object["spec"] = spec

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(gvk)
	err = c.Get(ctx, types.NamespacedName{Namespace: destNamespace, Name: name}, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Creating Snippets resource copy",
				"kind", kind,
				"namespace", destNamespace,
				"name", name)
			if err := c.Create(ctx, desired); err != nil {
				return false, fmt.Errorf("failed to create %s %s/%s: %w", kind, destNamespace, name, err)
			}
			return true, nil
		}
		return false, err
	}

	if !IsManagedByUs(existing) {
		logger.Info("Snippets resource exists but is not managed by us, skipping copy",
			"kind", kind,
			"namespace", destNamespace,
			"name", name)
		return false, nil
	}

	existing.SetLabels(desired.GetLabels())
	existing.SetAnnotations(desired.GetAnnotations())
	existing.Object["spec"] = spec
	logger.Info("Updating Snippets resource copy",
		"kind", kind,
		"namespace", destNamespace,
		"name", name)
	if err := c.Update(ctx, existing); err != nil {
		return false, fmt.Errorf("failed to update %s %s/%s: %w", kind, destNamespace, name, err)
	}
	return true, nil
}

// AddExtensionRefFilterAfterExistingKind inserts an ExtensionRef filter after the last filter of the same kind.
func AddExtensionRefFilterAfterExistingKind(
	httpRoute *gatewayv1.HTTPRoute,
	group string,
	kind string,
	name string,
) {
	if httpRoute == nil || name == "" {
		return
	}

	kindValue := gatewayv1.Kind(kind)
	for i := range httpRoute.Spec.Rules {
		rule := &httpRoute.Spec.Rules[i]
		exists := false
		lastIdx := -1
		for idx, filter := range rule.Filters {
			if filter.Type != gatewayv1.HTTPRouteFilterExtensionRef || filter.ExtensionRef == nil {
				continue
			}
			if string(filter.ExtensionRef.Group) != group {
				continue
			}
			if string(filter.ExtensionRef.Kind) != kind {
				continue
			}
			if string(filter.ExtensionRef.Name) == name {
				exists = true
				break
			}
			lastIdx = idx
		}
		if exists {
			continue
		}

		ref := gatewayv1.LocalObjectReference{
			Group: gatewayv1.Group(group),
			Kind:  kindValue,
			Name:  gatewayv1.ObjectName(name),
		}
		newFilter := gatewayv1.HTTPRouteFilter{
			Type:         gatewayv1.HTTPRouteFilterExtensionRef,
			ExtensionRef: &ref,
		}

		if lastIdx == -1 || lastIdx == len(rule.Filters)-1 {
			rule.Filters = append(rule.Filters, newFilter)
			continue
		}

		rule.Filters = append(rule.Filters, gatewayv1.HTTPRouteFilter{})
		copy(rule.Filters[lastIdx+2:], rule.Filters[lastIdx+1:])
		rule.Filters[lastIdx+1] = newFilter
	}
}

// AddExtensionRefFilters appends ExtensionRef filters to every HTTPRoute rule.
func AddExtensionRefFilters(
	httpRoute *gatewayv1.HTTPRoute,
	group string,
	kind string,
	names []string,
) {
	if httpRoute == nil || len(names) == 0 {
		return
	}

	kindValue := gatewayv1.Kind(kind)

	for i := range httpRoute.Spec.Rules {
		rule := &httpRoute.Spec.Rules[i]
		existing := make(map[string]struct{})
		for _, filter := range rule.Filters {
			if filter.Type != gatewayv1.HTTPRouteFilterExtensionRef || filter.ExtensionRef == nil {
				continue
			}
			if string(filter.ExtensionRef.Group) != group {
				continue
			}
			if string(filter.ExtensionRef.Kind) != kind {
				continue
			}
			existing[string(filter.ExtensionRef.Name)] = struct{}{}
		}

		for _, name := range names {
			if _, ok := existing[name]; ok {
				continue
			}
			ref := gatewayv1.LocalObjectReference{
				Group: gatewayv1.Group(group),
				Kind:  kindValue,
				Name:  gatewayv1.ObjectName(name),
			}
			rule.Filters = append(rule.Filters, gatewayv1.HTTPRouteFilter{
				Type:         gatewayv1.HTTPRouteFilterExtensionRef,
				ExtensionRef: &ref,
			})
		}
	}
}

func snippetsCRDNameForKind(kind string) (string, bool) {
	return extensionCRDNameForKind(kind)
}

func extensionCRDNameForKind(kind string) (string, bool) {
	switch kind {
	case SnippetsFilterKind:
		return SnippetsFilterCRDName, true
	case SnippetsPolicyKind:
		return SnippetsPolicyCRDName, true
	case AuthenticationFilterKind:
		return AuthenticationFilterCRDName, true
	case RequestHeaderModifierFilterKind:
		return RequestHeaderModifierCRDName, true
	case RateLimitPolicyKind:
		return RateLimitPolicyCRDName, true
	default:
		return "", false
	}
}

func getCRDVersion(ctx context.Context, c client.Reader, crdName string) (string, bool, error) {
	if cached, ok := crdVersionCache.Load(crdName); ok {
		entry := cached.(crdVersionCacheEntry)
		return entry.version, true, nil
	}

	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.Get(ctx, types.NamespacedName{Name: crdName}, crd); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}

	version, ok := pickCRDVersion(crd)
	if !ok {
		return "", false, nil
	}

	crdVersionCache.Store(crdName, crdVersionCacheEntry{version: version})
	return version, true, nil
}

// GetCRDVersion returns the storage/served version for a CRD name if installed.
func GetCRDVersion(ctx context.Context, c client.Reader, crdName string) (string, bool, error) {
	return getCRDVersion(ctx, c, crdName)
}

func pickCRDVersion(crd *apiextensionsv1.CustomResourceDefinition) (string, bool) {
	for _, version := range crd.Spec.Versions {
		if version.Storage {
			return version.Name, true
		}
	}
	for _, version := range crd.Spec.Versions {
		if version.Served {
			return version.Name, true
		}
	}
	return "", false
}

func copyStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

// SnippetsFilterGVK returns the GroupVersionKind used for SnippetsFilter watches.
func SnippetsFilterGVK() schema.GroupVersionKind {
	return ExtensionFilterGVK(SnippetsFilterKind, SnippetsFilterCRDName)
}

func ExtensionFilterGVK(kind, crdName string) schema.GroupVersionKind {
	version := "v1"
	if cached, ok := crdVersionCache.Load(crdName); ok {
		entry := cached.(crdVersionCacheEntry)
		if entry.version != "" {
			version = entry.version
		}
	}
	return schema.GroupVersionKind{Group: NginxGatewayGroup, Version: version, Kind: kind}
}
