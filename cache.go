// Package admit implements fixed-capacity in-memory caches under three
// eviction policies: LRU, LFU, and W-TinyLFU. See README.md for the
// reasoning behind picking W-TinyLFU as the default.
package admit

// Cache is a fixed-capacity key-value store with a pluggable eviction
// policy. All implementations are O(1) amortized per operation and are
// not safe for concurrent use; wrap with a mutex if you need that.
type Cache[K comparable, V any] interface {
	Get(key K) (V, bool)
	Set(key K, value V)
	Len() int
}

// node is the intrusive list element shared by every policy in this
// package. freq and seg are unused dead weight for policies that don't
// need them (LRU ignores both), which is a fine trade for not having
// three near-identical list implementations.
type node[K comparable, V any] struct {
	key   K
	value V
	prev  *node[K, V]
	next  *node[K, V]
	freq  int   // bucket index, used by LFU
	seg   uint8 // segment tag, used by W-TinyLFU
}

// list is a doubly linked list with sentinel head/tail nodes, so push and
// remove never have to special-case an empty list. front is the
// most-recently-pushed end; back is the eviction end.
type list[K comparable, V any] struct {
	head *node[K, V]
	tail *node[K, V]
	size int
}

func newList[K comparable, V any]() *list[K, V] {
	h := &node[K, V]{}
	t := &node[K, V]{}
	h.next = t
	t.prev = h
	return &list[K, V]{head: h, tail: t}
}

func (l *list[K, V]) pushFront(n *node[K, V]) {
	n.prev = l.head
	n.next = l.head.next
	l.head.next.prev = n
	l.head.next = n
	l.size++
}

func (l *list[K, V]) remove(n *node[K, V]) {
	n.prev.next = n.next
	n.next.prev = n.prev
	n.prev = nil
	n.next = nil
	l.size--
}

func (l *list[K, V]) moveToFront(n *node[K, V]) {
	l.remove(n)
	l.pushFront(n)
}

func (l *list[K, V]) back() *node[K, V] {
	if l.size == 0 {
		return nil
	}
	return l.tail.prev
}

func (l *list[K, V]) popBack() *node[K, V] {
	n := l.back()
	if n != nil {
		l.remove(n)
	}
	return n
}
