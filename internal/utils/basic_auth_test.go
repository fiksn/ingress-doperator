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

func TestBasicAuthFor(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        *BasicAuth
		wantErr     string
	}{
		{name: "no auth", annotations: map[string]string{AuthSecretAnnotation: "creds"}},
		{name: "basic",
			annotations: map[string]string{AuthTypeAnnotation: "basic", AuthSecretAnnotation: "creds",
				AuthRealmAnnotation: "Authentication Required - foo"},
			want: &BasicAuth{SecretName: "creds", Realm: "Authentication Required - foo"}},
		{name: "secret in the same namespace",
			annotations: map[string]string{AuthTypeAnnotation: "basic", AuthSecretAnnotation: "app/creds",
				AuthSecretTypeAnnotation: "auth-file"},
			want: &BasicAuth{SecretName: "creds"}},
		{name: "digest",
			annotations: map[string]string{AuthTypeAnnotation: "digest", AuthSecretAnnotation: "creds"},
			wantErr:     "only basic"},
		{name: "no secret",
			annotations: map[string]string{AuthTypeAnnotation: "basic"},
			wantErr:     "auth-secret is not set"},
		{name: "secret in another namespace",
			annotations: map[string]string{AuthTypeAnnotation: "basic", AuthSecretAnnotation: "other/creds"},
			wantErr:     "another namespace"},
		{name: "auth-map",
			annotations: map[string]string{AuthTypeAnnotation: "basic", AuthSecretAnnotation: "creds",
				AuthSecretTypeAnnotation: "auth-map"},
			wantErr: "auth-file"},
		{name: "realm with variable",
			annotations: map[string]string{AuthTypeAnnotation: "basic", AuthSecretAnnotation: "creds",
				AuthRealmAnnotation: "$host"},
			wantErr: "auth-realm"},
		{name: "realm with quote",
			annotations: map[string]string{AuthTypeAnnotation: "basic", AuthSecretAnnotation: "creds",
				AuthRealmAnnotation: `a"b`},
			wantErr: "auth-realm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BasicAuthFor(tt.annotations, "app")

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("BasicAuthFor() error = %v, want one mentioning %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("BasicAuthFor() unexpected error: %v", err)
			}
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Errorf("BasicAuthFor() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestBasicAuthFilterNameIsAValidLength(t *testing.T) {
	name := BasicAuthFilterName(strings.Repeat("a", 250))
	if len(name) > maxK8sNameLength || strings.HasSuffix(name, "-") {
		t.Errorf("BasicAuthFilterName() = %q (%d characters)", name, len(name))
	}
}
