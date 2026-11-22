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
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ingress-nginx basic authentication annotations.
const (
	AuthTypeAnnotation       = nginxIngressAnnotationPrefix + "auth-type"
	AuthSecretAnnotation     = nginxIngressAnnotationPrefix + "auth-secret"
	AuthSecretTypeAnnotation = nginxIngressAnnotationPrefix + "auth-secret-type"
	AuthRealmAnnotation      = nginxIngressAnnotationPrefix + "auth-realm"

	authTypeBasic      = "basic"
	authSecretTypeFile = "auth-file"
)

// BasicAuth is the HTTP basic authentication an Ingress requests with the ingress-nginx
// auth-type: basic annotations.
type BasicAuth struct {
	// SecretName is the Secret, in the Ingress's namespace, whose "auth" key holds an htpasswd file.
	SecretName string
	Realm      string
}

// RequestsAuthentication reports whether the Ingress's ingress-nginx annotations ask for basic or
// digest authentication.
func RequestsAuthentication(annotations map[string]string) bool {
	return strings.TrimSpace(annotations[AuthTypeAnnotation]) != ""
}

// BasicAuthFor returns the basic authentication requested by the ingress-nginx annotations of an
// Ingress in namespace, nil when they request none, and an error when they request authentication
// an NGINX Gateway Fabric AuthenticationFilter cannot provide: digest authentication, a Secret in
// another namespace, or the auth-map Secret format.
func BasicAuthFor(annotations map[string]string, namespace string) (*BasicAuth, error) {
	authType := strings.TrimSpace(annotations[AuthTypeAnnotation])
	if authType == "" {
		return nil, nil
	}
	if authType != authTypeBasic {
		return nil, fmt.Errorf("%s %q is not supported: only basic authentication is translated",
			AuthTypeAnnotation, authType)
	}
	secretRef := strings.TrimSpace(annotations[AuthSecretAnnotation])
	if secretRef == "" {
		return nil, fmt.Errorf("%s is not set", AuthSecretAnnotation)
	}
	secretName := secretRef
	if secretNamespace, name, found := strings.Cut(secretRef, "/"); found {
		if secretNamespace != namespace {
			return nil, fmt.Errorf("%s %q is in another namespace; NGINX Gateway Fabric only reads the "+
				"Secret from the Ingress's namespace %s", AuthSecretAnnotation, secretRef, namespace)
		}
		secretName = name
	}
	if secretType := strings.TrimSpace(annotations[AuthSecretTypeAnnotation]); secretType != "" &&
		secretType != authSecretTypeFile {
		return nil, fmt.Errorf("%s %q is not supported: NGINX Gateway Fabric needs the htpasswd file "+
			"format (auth-file) in the Secret's \"auth\" key", AuthSecretTypeAnnotation, secretType)
	}
	realm := annotations[AuthRealmAnnotation]
	if containsUnsafeSnippetChars(realm) || strings.ContainsAny(realm, "\"\\$") {
		return nil, fmt.Errorf("%s %q contains characters NGINX Gateway Fabric does not accept "+
			"(quotes, backslashes, $, ;, braces or line breaks)", AuthRealmAnnotation, realm)
	}
	return &BasicAuth{SecretName: secretName, Realm: realm}, nil
}

// BasicAuthFilterName returns the name of the AuthenticationFilter generated for an Ingress.
func BasicAuthFilterName(ingressName string) string {
	return truncateK8sName(fmt.Sprintf("automatic-%s-basic-auth", ingressName))
}

// EnsureBasicAuthFilter creates or updates the AuthenticationFilter of an Ingress in its namespace.
// It returns false when the AuthenticationFilter CRD (NGINX Gateway Fabric 2.4+) is not installed
// or an AuthenticationFilter of that name exists that ingress-doperator does not manage.
func EnsureBasicAuthFilter(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	owner client.Object,
	namespace, ingressName string,
	auth BasicAuth,
) (bool, error) {
	version, ok, err := getCRDVersion(ctx, c, AuthenticationFilterCRDName)
	if err != nil || !ok {
		return false, err
	}
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(schema.GroupVersionKind{
		Group: NginxGatewayGroup, Version: version, Kind: AuthenticationFilterKind,
	})
	desired.SetName(BasicAuthFilterName(ingressName))
	desired.SetNamespace(namespace)
	desired.Object["spec"] = map[string]interface{}{
		"type": "Basic",
		"basic": map[string]interface{}{
			"secretRef": map[string]interface{}{"name": auth.SecretName},
			"realm":     auth.Realm,
		},
	}
	return applyManagedResource(ctx, c, scheme, owner, desired, namespace+"/"+ingressName)
}
