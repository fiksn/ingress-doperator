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

package controller

import (
	"fmt"
	"path"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/fiksn/ingress-doperator/internal/utils"
)

// ParseWatchNamespaces parses the --watch-namespace value into namespace glob patterns.
// An empty value yields nil, which watches all namespaces.
func ParseWatchNamespaces(value string) ([]string, error) {
	patterns := utils.ParseCommaSeparatedList(value)
	for _, pattern := range patterns {
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("invalid namespace pattern %q: %w", pattern, err)
		}
	}
	return patterns, nil
}

// NamespaceFilter admits only objects whose namespace matches one of the given patterns.
func NamespaceFilter(patterns []string) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return inWatchedNamespaces(patterns, obj.GetNamespace())
	})
}

// inWatchedNamespaces reports whether namespace matches a pattern; no patterns watches all.
func inWatchedNamespaces(patterns []string, namespace string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		// Patterns are validated at startup, so Match cannot fail here.
		if matched, _ := path.Match(pattern, namespace); matched {
			return true
		}
	}
	return false
}
