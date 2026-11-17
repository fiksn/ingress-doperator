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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

const (
	namedPortRetryBase = time.Second
	namedPortRetryMax  = 30 * time.Second
)

// namedPortBackoff counts, per Ingress, the reconciles in a row whose named Service ports could
// not be resolved, so retries start fast (a Service usually appears shortly after its Ingress) and
// back off exponentially to namedPortRetryMax. The zero value is ready to use.
type namedPortBackoff struct {
	mu       sync.Mutex
	attempts map[types.NamespacedName]int
}

// next records a failed attempt for the Ingress and returns the delay before retrying:
// 1s, 2s, 4s, 8s, 16s, then 30s.
func (b *namedPortBackoff) next(ingress types.NamespacedName) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.attempts == nil {
		b.attempts = make(map[types.NamespacedName]int)
	}
	attempt := b.attempts[ingress]
	b.attempts[ingress] = attempt + 1
	delay := namedPortRetryBase
	for i := 0; i < attempt && delay < namedPortRetryMax; i++ {
		delay *= 2
	}
	return min(delay, namedPortRetryMax)
}

// reset forgets the failed attempts of the Ingress, e.g. once its ports resolve or it is deleted.
func (b *namedPortBackoff) reset(ingress types.NamespacedName) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.attempts, ingress)
}
