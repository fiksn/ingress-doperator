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
	mirrorTargetKey      = "mirror-target"
	mirrorRequestBodyKey = "mirror-request-body"
)

// mirrorHostRe matches a literal host name or IP with an optional port.
var mirrorHostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)

// mirrorState holds the request mirroring annotations of an Ingress.
type mirrorState struct {
	target      string
	requestBody string
}

func applyMirrorIngressAnnotationValue(state *nginxIngressSnippetState, suffix, value string) bool {
	switch suffix {
	case mirrorTargetKey:
		state.mirror.target = value
	case mirrorRequestBodyKey:
		state.mirror.requestBody = value
	default:
		return false
	}
	return true
}

// buildMirrorConfig emulates ingress-nginx request mirroring (mirror-target, mirror-request-body)
// with the NGINX mirror directive and an internal location proxying to the target. The target is
// proxied through a variable, so NGINX resolves its host per request with the resolver NGINX
// Gateway Fabric configures from its NginxProxy dnsResolver; a literal host would be resolved when
// the configuration loads, and an unresolvable one would make NGINX reject the whole Gateway's
// configuration. Without that resolver (dnsResolver false) mirroring is not translated.
func buildMirrorConfig(mirror mirrorState, dnsResolver bool, id string) (server, location, warnings []string) {
	target := strings.TrimSpace(mirror.target)
	if target == "" {
		return nil, nil, nil
	}
	if !dnsResolver {
		return nil, nil, []string{fmt.Sprintf("%s%s is not translated: mirroring needs a DNS resolver in NGINX "+
			"Gateway Fabric (NginxProxy spec.dnsResolver); configure it and run ingress-doperator with "+
			"--external-name-services=allow", nginxIngressAnnotationPrefix, mirrorTargetKey)}
	}
	scheme, host, ok := parseMirrorTarget(target)
	if !ok {
		return nil, nil, []string{fmt.Sprintf("%s%s %q is not an http(s) URL with a plain host; ignoring",
			nginxIngressAnnotationPrefix, mirrorTargetKey, target)}
	}

	withBody := true
	switch body := strings.ToLower(strings.TrimSpace(mirror.requestBody)); body {
	case "", "on":
	case "off":
		withBody = false
	default:
		warnings = append(warnings, fmt.Sprintf("%s%s %q is not on or off; mirroring the request body",
			nginxIngressAnnotationPrefix, mirrorRequestBodyKey, mirror.requestBody))
	}

	mirrorLocation := "/_doperator_mirror_" + id
	location = []string{fmt.Sprintf("mirror %s;", mirrorLocation)}
	targetVar := "$doperator_mirror_" + id
	lines := []string{
		nginxInternalDirective,
		fmt.Sprintf("set %s %s;", targetVar, target),
		fmt.Sprintf("proxy_set_header Host %s;", host),
	}
	if !withBody {
		location = append(location, "mirror_request_body off;")
		lines = append(lines, "proxy_pass_request_body off;", "proxy_set_header Content-Length \"\";")
	}
	if scheme == schemeHTTPS {
		lines = append(lines, "proxy_ssl_server_name on;", fmt.Sprintf("proxy_ssl_name %s;", hostWithoutPort(host)))
	}
	lines = append(lines, fmt.Sprintf("proxy_pass %s;", targetVar))
	server = []string{fmt.Sprintf("location = %s {\n%s\n}", mirrorLocation, indentNginxLines(lines))}
	return server, location, warnings
}

// parseMirrorTarget checks that target is an http(s) URL whose host (with optional port) is
// literal: the host ends at the first '/', '?' or '$', so NGINX variables such as $request_uri may
// only follow it. It returns the scheme
// and the host.
func parseMirrorTarget(target string) (scheme, host string, ok bool) {
	if containsUnsafeSnippetChars(target) || strings.ContainsAny(target, "\"'\\ \t") {
		return "", "", false
	}
	scheme, rest, found := strings.Cut(target, "://")
	if !found || (scheme != "http" && scheme != schemeHTTPS) {
		return "", "", false
	}
	host = rest
	if i := strings.IndexAny(rest, "/?$"); i >= 0 {
		host = rest[:i]
	}
	if !mirrorHostRe.MatchString(host) {
		return "", "", false
	}
	return scheme, host, true
}

func hostWithoutPort(host string) string {
	name, _, _ := strings.Cut(host, ":")
	return name
}
