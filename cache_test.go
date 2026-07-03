package admit

import "testing"

func identity(k int) uint64 { return uint64(k) }

func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	c := NewLRU[int, string](2)
	c.Set(1, "a")
	c.Set(2, "b")
	c.Get(1)      // 1 is now more recent than 2
	c.Set(3, "c") // should evict 2, not 1

	if _, ok := c.Get(2); ok {
		t.Fatal("key 2 should have been evicted")
	}
	if v, ok := c.Get(1); !ok || v != "a" {
		t.Fatalf("key 1 should still be present, got %q, %v", v, ok)
	}
	if v, ok := c.Get(3); !ok || v != "c" {
		t.Fatalf("key 3 should be present, got %q, %v", v, ok)
	}
}

func TestLFUEvictsLeastFrequentlyUsed(t *testing.T) {
	c := NewLFU[int, string](2)
	c.Set(1, "a")
	c.Set(2, "b")
	c.Get(1)
	c.Get(1)      // key 1 now has higher frequency than key 2
	c.Set(3, "c") // should evict 2, the least frequently used, even though it's more recent than 1

	if _, ok := c.Get(2); ok {
		t.Fatal("key 2 should have been evicted (lowest frequency)")
	}
	if _, ok := c.Get(1); !ok {
		t.Fatal("key 1 should still be present (highest frequency)")
	}
}

func TestWTinyLFURejectsOneHitWondersOverAFrequentKey(t *testing.T) {
	const capacity = 50
	c := NewWTinyLFU[int, int](capacity, identity)

	const hotKey = 9999
	c.Set(hotKey, hotKey)
	for i := 0; i < capacity-1; i++ {
		c.Set(i, i) // fill every other slot so main is at capacity
	}
	for i := 0; i < 20; i++ {
		c.Get(hotKey) // earn frequency and promotion to the protected segment
	}
	if _, ok := c.Get(hotKey); !ok {
		t.Fatal("hot key should be resident before the pollution phase")
	}

	for i := 100_000; i < 101_000; i++ {
		c.Set(i, i) // a flood of keys that are each touched exactly once
	}

	if _, ok := c.Get(hotKey); !ok {
		t.Fatal("hot key was evicted by a flood of one-hit-wonders; admission filter failed")
	}
}

func TestCachesRespectCapacity(t *testing.T) {
	const capacity = 20
	caches := map[string]Cache[int, int]{
		"LRU":       NewLRU[int, int](capacity),
		"LFU":       NewLFU[int, int](capacity),
		"W-TinyLFU": NewWTinyLFU[int, int](capacity, identity),
	}
	for name, c := range caches {
		for i := 0; i < capacity*5; i++ {
			c.Set(i, i)
		}
		if c.Len() > capacity {
			t.Errorf("%s: Len() = %d, want <= %d", name, c.Len(), capacity)
		}
	}
}

func TestCachesReturnStoredValue(t *testing.T) {
	caches := map[string]Cache[string, int]{
		"LRU":       NewLRU[string, int](10),
		"LFU":       NewLFU[string, int](10),
		"W-TinyLFU": NewWTinyLFU[string, int](10, HashString),
	}
	for name, c := range caches {
		c.Set("x", 42)
		v, ok := c.Get("x")
		if !ok || v != 42 {
			t.Errorf("%s: Get(x) = %d, %v, want 42, true", name, v, ok)
		}
		if _, ok := c.Get("missing"); ok {
			t.Errorf("%s: Get(missing) returned ok=true", name)
		}
	}
}

func benchmarkGetSet(b *testing.B, c Cache[int, int]) {
	const workingSet = 10_000
	for i := 0; i < workingSet; i++ {
		c.Set(i, i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k := i % workingSet
		if _, ok := c.Get(k); !ok {
			c.Set(k, k)
		}
	}
}

func BenchmarkLRU(b *testing.B)      { benchmarkGetSet(b, NewLRU[int, int](1000)) }
func BenchmarkLFU(b *testing.B)      { benchmarkGetSet(b, NewLFU[int, int](1000)) }
func BenchmarkWTinyLFU(b *testing.B) { benchmarkGetSet(b, NewWTinyLFU[int, int](1000, identity)) }
