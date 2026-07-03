package admit

// LRU is a fixed-capacity cache that evicts the least-recently-used key.
// It's the default everyone reaches for and the baseline everything else
// in this package is measured against.
type LRU[K comparable, V any] struct {
	capacity int
	items    map[K]*node[K, V]
	order    *list[K, V]
}

func NewLRU[K comparable, V any](capacity int) *LRU[K, V] {
	if capacity < 1 {
		capacity = 1
	}
	return &LRU[K, V]{
		capacity: capacity,
		items:    make(map[K]*node[K, V], capacity),
		order:    newList[K, V](),
	}
}

func (c *LRU[K, V]) Get(key K) (V, bool) {
	n, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.order.moveToFront(n)
	return n.value, true
}

func (c *LRU[K, V]) Set(key K, value V) {
	if n, ok := c.items[key]; ok {
		n.value = value
		c.order.moveToFront(n)
		return
	}
	if len(c.items) >= c.capacity {
		victim := c.order.popBack()
		if victim != nil {
			delete(c.items, victim.key)
		}
	}
	n := &node[K, V]{key: key, value: value}
	c.order.pushFront(n)
	c.items[key] = n
}

func (c *LRU[K, V]) Len() int {
	return len(c.items)
}
