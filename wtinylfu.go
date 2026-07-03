package admit

const (
	segWindow uint8 = iota
	segProbation
	segProtected
)

// windowPercent is the fraction of total capacity reserved for the
// admission window. 1% matches Caffeine's default and the original
// TinyLFU paper's recommendation: big enough to absorb a burst of
// one-hit-wonders without thrashing, small enough that it can't crowd
// out the frequency-filtered main cache.
const windowPercent = 1

// protectedPercent is the share of main capacity that's immune to plain
// LRU eviction once a key has been accessed twice. 80/20 is Caffeine's
// SLRU split; the second access is what earns protection; a hard, not a
// gradual, promotion, is one of the two things that make TinyLFU cheap to
// reason about (the other is the sketch itself).
const protectedPercent = 80

// WTinyLFU is a window-admission cache: a small LRU window absorbs
// recency spikes and one-hit wonders, and only candidates that beat the
// estimated frequency of the current main-cache victim get promoted out
// of it. Main itself is a Segmented LRU (probation/protected), so a key
// has to be accessed twice before it's shielded from ordinary eviction.
//
// This is the policy Caffeine and Ristretto both ship as their default.
// See README.md for the numbers backing that choice.
//
// Keys are hashed by the caller-supplied hash function rather than via
// reflection, so pick something with good avalanche behavior for your
// key type (hash/maphash.String works well for strings).
type WTinyLFU[K comparable, V any] struct {
	windowCap    int
	protectedCap int
	mainCap      int
	hash         func(K) uint64

	items     map[K]*node[K, V]
	window    *list[K, V]
	probation *list[K, V]
	protected *list[K, V]

	sk *sketch
}

func NewWTinyLFU[K comparable, V any](capacity int, hash func(K) uint64) *WTinyLFU[K, V] {
	if capacity < 1 {
		capacity = 1
	}
	windowCap := capacity * windowPercent / 100
	if windowCap < 1 {
		windowCap = 1
	}
	if windowCap > capacity {
		windowCap = capacity
	}
	mainCap := capacity - windowCap
	return &WTinyLFU[K, V]{
		windowCap:    windowCap,
		mainCap:      mainCap,
		protectedCap: mainCap * protectedPercent / 100,
		hash:         hash,
		items:        make(map[K]*node[K, V], capacity),
		window:       newList[K, V](),
		probation:    newList[K, V](),
		protected:    newList[K, V](),
		sk:           newSketch(capacity),
	}
}

func (c *WTinyLFU[K, V]) Get(key K) (V, bool) {
	c.sk.Add(c.hash(key))
	n, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.access(n)
	return n.value, true
}

func (c *WTinyLFU[K, V]) Set(key K, value V) {
	h := c.hash(key)
	if n, ok := c.items[key]; ok {
		n.value = value
		c.sk.Add(h)
		c.access(n)
		return
	}
	c.sk.Add(h)
	n := &node[K, V]{key: key, value: value, seg: segWindow}
	c.window.pushFront(n)
	c.items[key] = n

	if c.window.size > c.windowCap {
		c.admitToMain(c.window.popBack())
	}
}

func (c *WTinyLFU[K, V]) Len() int {
	return len(c.items)
}

// access records a hit against an already-resident node, promoting it
// out of probation on its second access and demoting protected's
// coldest entry back if that pushes protected over its share of main.
func (c *WTinyLFU[K, V]) access(n *node[K, V]) {
	switch n.seg {
	case segWindow:
		c.window.moveToFront(n)
	case segProtected:
		c.protected.moveToFront(n)
	case segProbation:
		c.probation.remove(n)
		n.seg = segProtected
		c.protected.pushFront(n)
		if c.protected.size > c.protectedCap {
			demoted := c.protected.popBack()
			demoted.seg = segProbation
			c.probation.pushFront(demoted)
		}
	}
}

// admitToMain is the admission filter: a key evicted from the window
// either slots into main for free (still warming up) or has to win a
// frequency comparison against main's current probation victim.
func (c *WTinyLFU[K, V]) admitToMain(candidate *node[K, V]) {
	if c.probation.size+c.protected.size < c.mainCap {
		candidate.seg = segProbation
		c.probation.pushFront(candidate)
		return
	}
	victim := c.probation.back()
	if victim == nil {
		victim = c.protected.back()
	}
	if victim == nil || c.sk.Estimate(c.hash(candidate.key)) <= c.sk.Estimate(c.hash(victim.key)) {
		delete(c.items, candidate.key)
		return
	}
	if victim.seg == segProbation {
		c.probation.remove(victim)
	} else {
		c.protected.remove(victim)
	}
	delete(c.items, victim.key)
	candidate.seg = segProbation
	c.probation.pushFront(candidate)
}
