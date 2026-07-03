package admit

// LFU is a fixed-capacity cache that evicts the least-frequently-used
// key, breaking ties by recency within the tied frequency bucket. It
// never forgets a hot key just because something newer showed up, which
// is the thing LRU gets wrong under scans, but it pays for that by never
// forgetting a key that used to be hot either. See README for why that
// makes it a bad default despite winning on paper.
//
// Buckets-of-lists keeps every operation O(1): items groups nodes by
// current frequency, minFreq tracks the bucket to evict from next.
type LFU[K comparable, V any] struct {
	capacity int
	items    map[K]*node[K, V]
	buckets  map[int]*list[K, V]
	minFreq  int
}

func NewLFU[K comparable, V any](capacity int) *LFU[K, V] {
	if capacity < 1 {
		capacity = 1
	}
	return &LFU[K, V]{
		capacity: capacity,
		items:    make(map[K]*node[K, V], capacity),
		buckets:  make(map[int]*list[K, V]),
	}
}

func (c *LFU[K, V]) bucket(freq int) *list[K, V] {
	l, ok := c.buckets[freq]
	if !ok {
		l = newList[K, V]()
		c.buckets[freq] = l
	}
	return l
}

// touch promotes n to its next frequency bucket and advances minFreq past
// any bucket that touch just emptied.
func (c *LFU[K, V]) touch(n *node[K, V]) {
	old := n.freq
	c.buckets[old].remove(n)
	if old == c.minFreq && c.buckets[old].size == 0 {
		c.minFreq++
	}
	n.freq++
	c.bucket(n.freq).pushFront(n)
}

func (c *LFU[K, V]) Get(key K) (V, bool) {
	n, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.touch(n)
	return n.value, true
}

func (c *LFU[K, V]) Set(key K, value V) {
	if n, ok := c.items[key]; ok {
		n.value = value
		c.touch(n)
		return
	}
	if len(c.items) >= c.capacity {
		victim := c.buckets[c.minFreq].popBack()
		if victim != nil {
			delete(c.items, victim.key)
		}
	}
	n := &node[K, V]{key: key, value: value, freq: 1}
	c.bucket(1).pushFront(n)
	c.items[key] = n
	c.minFreq = 1
}

func (c *LFU[K, V]) Len() int {
	return len(c.items)
}
