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
	"strings"
)

// IngressClassAnnotationsRule maps a glob pattern to annotations.
type IngressClassAnnotationsRule struct {
	Pattern     string
	Annotations map[string]string
}

// ParseIngressClassAnnotationsByClass parses rules of the form:
// pattern:key=value,key=value;pattern2:key=value
func ParseIngressClassAnnotationsByClass(raw string) ([]IngressClassAnnotationsRule, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	entries := strings.Split(raw, ";")
	rules := make([]IngressClassAnnotationsRule, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid annotations-by-class entry %q (expected pattern:kv,kv)", entry)
		}
		pattern := strings.TrimSpace(parts[0])
		if pattern == "" {
			return nil, fmt.Errorf("invalid annotations-by-class entry %q (empty pattern)", entry)
		}
		annotationsRaw := strings.TrimSpace(parts[1])
		if annotationsRaw == "" {
			return nil, fmt.Errorf("invalid annotations-by-class entry %q (missing annotations)", entry)
		}
		annotations := make(map[string]string)
		for _, pair := range strings.Split(annotationsRaw, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) != 2 {
				return nil, fmt.Errorf("invalid annotations-by-class pair %q (expected key=value)", pair)
			}
			key := strings.TrimSpace(kv[0])
			if key == "" {
				return nil, fmt.Errorf("invalid annotations-by-class pair %q (empty key)", pair)
			}
			annotations[key] = strings.TrimSpace(kv[1])
		}
		if len(annotations) == 0 {
			return nil, fmt.Errorf("invalid annotations-by-class entry %q (no valid annotations)", entry)
		}
		rules = append(rules, IngressClassAnnotationsRule{
			Pattern:     pattern,
			Annotations: annotations,
		})
	}
	return rules, nil
}
