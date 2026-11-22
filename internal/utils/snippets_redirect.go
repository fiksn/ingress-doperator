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
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	permanentRedirectKey     = "permanent-redirect"
	permanentRedirectCodeKey = "permanent-redirect-code"
	temporalRedirectKey      = "temporal-redirect"
	temporalRedirectCodeKey  = "temporal-redirect-code"
	appRootKey               = "app-root"
)

// appRootRe matches an absolute path without characters NGINX would interpret.
var appRootRe = regexp.MustCompile(`^/[A-Za-z0-9\-._~/%]*$`)

// redirectState holds the redirect annotations of an Ingress.
type redirectState struct {
	permanent     string
	permanentCode string
	temporal      string
	temporalCode  string
	appRoot       string
}

func applyRedirectIngressAnnotationValue(state *nginxIngressSnippetState, suffix, value string) bool {
	switch suffix {
	case permanentRedirectKey:
		state.redirect.permanent = value
	case permanentRedirectCodeKey:
		state.redirect.permanentCode = value
	case temporalRedirectKey:
		state.redirect.temporal = value
	case temporalRedirectCodeKey:
		state.redirect.temporalCode = value
	case appRootKey:
		state.redirect.appRoot = value
	default:
		return false
	}
	return true
}

// buildRedirectConfig emulates the ingress-nginx redirect annotations. temporal-redirect wins over
// permanent-redirect and both answer every request of the Ingress with "return <code> <url>", so,
// as with ingress-nginx, the request path is not appended unless the URL uses NGINX variables such
// as $request_uri. app-root redirects requests for "/" of the hosts to the given path.
func buildRedirectConfig(redirect redirectState) (server, location, warnings []string) {
	if line, warning := redirectReturn(redirect); line != "" || warning != "" {
		if line != "" {
			location = append(location, line)
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	if appRoot := redirect.appRoot; appRoot != "" {
		if appRootRe.MatchString(appRoot) {
			server = append(server, fmt.Sprintf("if ($uri = /) { return 302 $scheme://$http_host%s; }", appRoot))
		} else {
			warnings = append(warnings, fmt.Sprintf("%s%s %q is not an absolute path; ignoring",
				nginxIngressAnnotationPrefix, appRootKey, appRoot))
		}
	}
	return server, location, warnings
}

func redirectReturn(redirect redirectState) (line, warning string) {
	key, target, codeKey, rawCode := permanentRedirectKey, redirect.permanent, permanentRedirectCodeKey,
		redirect.permanentCode
	defaultCode, maxCode := http.StatusMovedPermanently, http.StatusPermanentRedirect
	if redirect.temporal != "" {
		key, target, codeKey, rawCode = temporalRedirectKey, redirect.temporal, temporalRedirectCodeKey,
			redirect.temporalCode
		defaultCode, maxCode = http.StatusFound, http.StatusTemporaryRedirect
	}
	if target == "" {
		return "", ""
	}
	if !isValidRedirectURL(target) {
		return "", fmt.Sprintf("%s%s %q is not an http(s) URL; ignoring", nginxIngressAnnotationPrefix, key, target)
	}
	code := defaultCode
	if rawCode != "" {
		parsed, err := strconv.Atoi(rawCode)
		if err == nil && parsed >= http.StatusMultipleChoices && parsed <= maxCode {
			code = parsed
		} else {
			warning = fmt.Sprintf("%s%s %q is not a redirect status code from 300 to %d; using %d",
				nginxIngressAnnotationPrefix, codeKey, rawCode, maxCode, defaultCode)
		}
	}
	return fmt.Sprintf("return %d \"%s\";", code, target), warning
}

func isValidRedirectURL(target string) bool {
	if containsUnsafeSnippetChars(target) || strings.ContainsAny(target, "\"'\\ \t") {
		return false
	}
	parsed, err := url.Parse(target)
	return err == nil && (parsed.Scheme == schemeHTTP || parsed.Scheme == schemeHTTPS) && parsed.Host != ""
}
