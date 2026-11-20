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
	"fmt"
	"testing"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func headerList(headers []gatewayv1.HTTPHeader) string {
	out := ""
	for _, header := range headers {
		out += fmt.Sprintf("[%s=%s]", header.Name, header.Value)
	}
	return out
}

func TestParseHeaderNameValueList(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "  ", ""},
		{"single", "X-Env=prod", "[X-Env=prod]"},
		{"several", "X-Env=prod, X-Team=core", "[X-Env=prod][X-Team=core]"},
		{"value keeps later =", "Authorization=Basic dXNlcjpwYXNz==", "[Authorization=Basic dXNlcjpwYXNz==]"},
		{"quoted comma", `X-Robots-Tag="noindex, nofollow"`, "[X-Robots-Tag=noindex, nofollow]"},
		{"quoted among others", `X-Env=prod,X-Robots-Tag="noindex, nofollow",X-Team=core`,
			"[X-Env=prod][X-Robots-Tag=noindex, nofollow][X-Team=core]"},
		{"escaped quote and backslash", `X-Note="say \"hi\", C:\\tmp"`, `[X-Note=say "hi", C:\tmp]`},
		{"quotes only around a part stay", `X-Note=a"b,c"d`, `[X-Note=a"b,c"d]`},
		{"empty quoted value", `X-Empty=""`, "[X-Empty=]"},
		{"unclosed quote falls back to splitting on every comma", `X-A="x, y,X-B=z`, `[X-A="x][X-B=z]`},
		{"entry without = is skipped", "novalue, X-Env=prod", "[X-Env=prod]"},
		{"duplicates removed", "X-Env=prod,X-Env=prod", "[X-Env=prod]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := headerList(parseHeaderNameValueList(tt.raw)); got != tt.want {
				t.Errorf("parseHeaderNameValueList(%q) = %s, want %s", tt.raw, got, tt.want)
			}
		})
	}
}

func TestBuildHeaderModifierFilters_QuotedResponseHeader(t *testing.T) {
	_, response := buildHeaderModifierFilters(map[string]string{
		ResponseHeaderSetAnnotation: `X-Robots-Tag="noindex, nofollow"`,
	})

	if response == nil || response.ResponseHeaderModifier == nil {
		t.Fatal("no ResponseHeaderModifier filter")
	}
	if got := headerList(response.ResponseHeaderModifier.Set); got != "[X-Robots-Tag=noindex, nofollow]" {
		t.Errorf("Set = %s, want [X-Robots-Tag=noindex, nofollow]", got)
	}
}
