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
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ReconcileCacheConfigMapBaseName = "ingress-doperator-reconcile-cache"
	ReconcileCacheShardCount        = 16
	ReconcileCacheTTL               = 24 * time.Hour
)

type ReconcileCacheEntry struct {
	ResourceVersion string
	UpdatedAtUnix   int64
}

func LoadReconcileCacheSharded(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	baseName string,
	shardCount int,
) (map[string]ReconcileCacheEntry, error) {
	if shardCount <= 0 {
		shardCount = 1
	}
	out := make(map[string]ReconcileCacheEntry)
	cutoff := time.Now().Add(-ReconcileCacheTTL).Unix()
	for shard := 0; shard < shardCount; shard++ {
		cm := &corev1.ConfigMap{}
		name := fmt.Sprintf("%s-%d", baseName, shard)
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if cm.Data == nil {
			continue
		}
		for key, value := range cm.Data {
			entry, ok := parseReconcileCacheEntry(value)
			if !ok {
				continue
			}
			if entry.UpdatedAtUnix < cutoff {
				continue
			}
			out[key] = entry
		}
	}
	return out, nil
}

func SaveReconcileCacheSharded(
	ctx context.Context,
	c client.Client,
	namespace string,
	baseName string,
	shardCount int,
	data map[string]ReconcileCacheEntry,
) error {
	if shardCount <= 0 {
		shardCount = 1
	}
	shards := make(map[int]map[string]string, shardCount)
	for key, value := range data {
		shard := reconcileCacheShardForKey(key, shardCount)
		if shards[shard] == nil {
			shards[shard] = make(map[string]string)
		}
		shards[shard][key] = formatReconcileCacheEntry(value)
	}

	for shard := 0; shard < shardCount; shard++ {
		shardData := shards[shard]
		if shardData == nil {
			shardData = make(map[string]string)
		}
		cm := &corev1.ConfigMap{}
		name := fmt.Sprintf("%s-%d", baseName, shard)
		key := client.ObjectKey{Namespace: namespace, Name: name}
		err := c.Get(ctx, key, cm)
		if err != nil {
			if apierrors.IsNotFound(err) {
				newCM := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: namespace,
					},
					Data: copyStringMap(shardData),
				}
				if err := c.Create(ctx, newCM); err != nil {
					return fmt.Errorf("failed to create reconcile cache configmap: %w", err)
				}
				continue
			}
			return err
		}

		cm.Data = copyStringMap(shardData)
		if err := c.Update(ctx, cm); err != nil {
			return fmt.Errorf("failed to update reconcile cache configmap: %w", err)
		}
	}
	return nil
}

func reconcileCacheShardForKey(key string, shardCount int) int {
	if shardCount <= 1 {
		return 0
	}
	h := fnv32a(key)
	return int(h % uint32(shardCount))
}

func fnv32a(s string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	var hash uint32 = offset32
	for i := 0; i < len(s); i++ {
		hash ^= uint32(s[i])
		hash *= prime32
	}
	return hash
}

func ReconcileCacheKey(id string) string {
	key := strings.TrimSpace(id)
	key = strings.ReplaceAll(key, "/", "__")
	if key == "" {
		return ""
	}
	return key
}

func parseReconcileCacheEntry(raw string) (ReconcileCacheEntry, bool) {
	parts := strings.Split(raw, "|")
	if len(parts) != 2 {
		return ReconcileCacheEntry{}, false
	}
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return ReconcileCacheEntry{}, false
	}
	return ReconcileCacheEntry{
		ResourceVersion: parts[0],
		UpdatedAtUnix:   ts,
	}, true
}

func formatReconcileCacheEntry(entry ReconcileCacheEntry) string {
	return fmt.Sprintf("%s|%d", entry.ResourceVersion, entry.UpdatedAtUnix)
}
