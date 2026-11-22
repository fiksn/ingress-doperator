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
	"net"
	"strconv"
	"strings"
)

const (
	limitConnectionsKey     = "limit-connections"
	limitRPSKey             = "limit-rps"
	limitRPMKey             = "limit-rpm"
	limitBurstMultiplierKey = "limit-burst-multiplier"
	limitWhitelistKey       = "limit-whitelist"
	limitAllowlistKey       = "limit-allowlist"
	limitRateKey            = "limit-rate"
	limitRateAfterKey       = "limit-rate-after"

	// ingress-nginx defaults: bursts of 5 times the limit, 5 MB zones keyed by client address.
	defaultLimitBurstMultiplier = 5
	limitZoneSize               = "5m"
	limitClientKey              = "$binary_remote_addr"
)

// rateLimitState holds the rate limiting annotations of an Ingress.
type rateLimitState struct {
	connections     string
	rps             string
	rpm             string
	burstMultiplier string
	allowlist       string
	rate            string
	rateAfter       string
}

func applyRateLimitIngressAnnotationValue(state *nginxIngressSnippetState, suffix, value string) bool {
	switch suffix {
	case limitConnectionsKey:
		state.rateLimit.connections = value
	case limitRPSKey:
		state.rateLimit.rps = value
	case limitRPMKey:
		state.rateLimit.rpm = value
	case limitBurstMultiplierKey:
		state.rateLimit.burstMultiplier = value
	case limitWhitelistKey, limitAllowlistKey:
		state.rateLimit.allowlist = value
	case limitRateKey:
		state.rateLimit.rate = value
	case limitRateAfterKey:
		state.rateLimit.rateAfter = value
	default:
		return false
	}
	return true
}

// buildRateLimitConfig emulates the ingress-nginx rate limiting annotations with the zones and
// limits ingress-nginx generates: limit_conn and limit_req zones per Ingress keyed by the client
// address (exempting the limit-whitelist ranges), bursts of limit-burst-multiplier times the limit
// without delay, and limit_rate / limit_rate_after in kilobytes per second.
func buildRateLimitConfig(limits rateLimitState, id string) (httpLines, location, warnings []string) {
	positive := func(key, raw string) int {
		if raw == "" {
			return 0
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			warnings = append(warnings, fmt.Sprintf("%s%s %q is not a positive integer; ignoring",
				nginxIngressAnnotationPrefix, key, raw))
			return 0
		}
		return n
	}
	connections := positive(limitConnectionsKey, limits.connections)
	rps := positive(limitRPSKey, limits.rps)
	rpm := positive(limitRPMKey, limits.rpm)
	multiplier := positive(limitBurstMultiplierKey, limits.burstMultiplier)
	if multiplier == 0 {
		multiplier = defaultLimitBurstMultiplier
	}

	if connections > 0 || rps > 0 || rpm > 0 {
		key, allowlistLines, allowlistWarnings := rateLimitKey(limits.allowlist, id)
		httpLines = append(httpLines, allowlistLines...)
		warnings = append(warnings, allowlistWarnings...)
		zone := "doperator_" + id
		if connections > 0 {
			httpLines = append(httpLines, fmt.Sprintf("limit_conn_zone %s zone=%s_conn:%s;", key, zone, limitZoneSize))
			location = append(location, fmt.Sprintf("limit_conn %s_conn %d;", zone, connections))
		}
		for _, req := range []struct {
			limit int
			name  string
			unit  string
		}{{rps, "rps", "s"}, {rpm, "rpm", "m"}} {
			if req.limit == 0 {
				continue
			}
			httpLines = append(httpLines, fmt.Sprintf("limit_req_zone %s zone=%s_%s:%s rate=%dr/%s;",
				key, zone, req.name, limitZoneSize, req.limit, req.unit))
			location = append(location, fmt.Sprintf("limit_req zone=%s_%s burst=%d nodelay;",
				zone, req.name, req.limit*multiplier))
		}
	}
	if rateAfter := positive(limitRateAfterKey, limits.rateAfter); rateAfter > 0 {
		location = append(location, fmt.Sprintf("limit_rate_after %dk;", rateAfter))
	}
	if rate := positive(limitRateKey, limits.rate); rate > 0 {
		location = append(location, fmt.Sprintf("limit_rate %dk;", rate))
	}
	return httpLines, location, warnings
}

// rateLimitKey returns the variable the limit zones are keyed by: the client address, or, with
// allowed ranges, a variable that is empty for clients in them (NGINX does not limit empty keys).
func rateLimitKey(raw, id string) (key string, httpLines, warnings []string) {
	var ranges []string
	for _, cidr := range parseCommaSeparated(raw) {
		if !isIPOrCIDR(cidr) {
			warnings = append(warnings, fmt.Sprintf("%s%s: ignoring invalid address range %q",
				nginxIngressAnnotationPrefix, limitWhitelistKey, cidr))
			continue
		}
		ranges = append(ranges, cidr)
	}
	if len(ranges) == 0 {
		return limitClientKey, nil, warnings
	}
	allowed := "$doperator_limit_allowed_" + id
	key = "$doperator_limit_key_" + id
	geo := []string{"default 0;"}
	for _, cidr := range ranges {
		geo = append(geo, cidr+" 1;")
	}
	httpLines = []string{
		fmt.Sprintf("geo $remote_addr %s {\n%s\n}", allowed, indentNginxLines(geo)),
		fmt.Sprintf("map %s %s {\n%s\n}", allowed, key,
			indentNginxLines([]string{"0 " + limitClientKey + ";", "1 \"\";"})),
	}
	return key, httpLines, warnings
}

func isIPOrCIDR(value string) bool {
	if strings.Contains(value, "/") {
		_, _, err := net.ParseCIDR(value)
		return err == nil
	}
	return net.ParseIP(value) != nil
}
