// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import "container/list"

// lru is a minimal bounded cache. The symbolizer keeps parsed symbol
// tables in one: binaries are few but big, so the bound is on entries and
// the least recently used table is released first. Not safe for
// concurrent use; each drain loop owns its caches.
type lru[K comparable, V any] struct {
	cap   int
	order *list.List // front = most recently used
	items map[K]*list.Element
}

type lruEntry[K comparable, V any] struct {
	key K
	val V
}

func newLRU[K comparable, V any](capacity int) *lru[K, V] {
	if capacity < 1 {
		capacity = 1
	}
	return &lru[K, V]{cap: capacity, order: list.New(), items: make(map[K]*list.Element, capacity)}
}

func (c *lru[K, V]) get(k K) (V, bool) {
	if e, ok := c.items[k]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*lruEntry[K, V]).val, true
	}
	var zero V
	return zero, false
}

func (c *lru[K, V]) put(k K, v V) {
	if e, ok := c.items[k]; ok {
		e.Value.(*lruEntry[K, V]).val = v
		c.order.MoveToFront(e)
		return
	}
	c.items[k] = c.order.PushFront(&lruEntry[K, V]{key: k, val: v})
	for c.order.Len() > c.cap {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*lruEntry[K, V]).key)
	}
}

func (c *lru[K, V]) len() int { return c.order.Len() }
