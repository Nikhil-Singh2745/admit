package admit

import "math/rand"

// Trace is a sequence of key accesses used to drive a cache and measure
// hit ratio. Keys are plain ints so every policy under test can share a
// trivial identity hash.
type Trace []int

// ZipfianTrace generates n accesses over a universe of keySpace distinct
// keys, skewed by s. This models real traffic: a small set of keys
// account for most requests. It's built on math/rand's Zipf generator
// (Gray et al.'s algorithm) rather than a hand-rolled one, there's no
// reason to reimplement a distribution the stdlib already gets right.
// s must be > 1; higher s concentrates traffic on fewer keys.
func ZipfianTrace(seed int64, n, keySpace int, s float64) Trace {
	r := rand.New(rand.NewSource(seed))
	z := rand.NewZipf(r, s, 1, uint64(keySpace-1))
	trace := make(Trace, n)
	for i := range trace {
		trace[i] = int(z.Uint64())
	}
	return trace
}

// ScanPollutedTrace interleaves Zipfian hot traffic with bursts of
// sequential, never-repeated "cold" keys - the pattern that breaks plain
// LRU. A full-table scan evicts the whole hot set because every scanned
// key looks most-recently-used even though none of them will be touched
// again. scanEvery is the number of hot accesses between scan bursts;
// scanLen is the burst length. Cold keys are drawn from above keySpace
// so they never collide with the hot working set.
//
// It also returns hot, a same-length mask marking which entries are
// Zipfian (repeatable) accesses. Cold scan keys are misses for every
// policy by construction, so pooling them into a raw hit ratio just
// dilutes the signal by a constant the eviction policy has no control
// over. Hit ratio should be measured over hot[i]==true entries: that's
// what shows how much collateral damage the scan actually did to the
// working set.
func ScanPollutedTrace(seed int64, n, keySpace int, s float64, scanEvery, scanLen int) (trace Trace, hot []bool) {
	r := rand.New(rand.NewSource(seed))
	z := rand.NewZipf(r, s, 1, uint64(keySpace-1))
	trace = make(Trace, 0, n)
	hot = make([]bool, 0, n)
	coldKey := keySpace
	for len(trace) < n {
		for i := 0; i < scanEvery && len(trace) < n; i++ {
			trace = append(trace, int(z.Uint64()))
			hot = append(hot, true)
		}
		for i := 0; i < scanLen && len(trace) < n; i++ {
			trace = append(trace, coldKey)
			hot = append(hot, false)
			coldKey++
		}
	}
	return trace, hot
}

// DriftingZipfianTrace models a workload whose hot set rotates once,
// halfway through: the first half is Zipfian over one key range, the
// second half Zipfian over a disjoint range of the same size. This is
// what actually breaks plain LFU. A key's frequency count from the first
// half never decays, so once traffic moves on, LFU keeps the stale hot
// set pinned in cache and starves the new one - exactly the failure mode
// TinyLFU's aging sketch exists to avoid. postDrift marks the second-half
// entries, since that's the only period where the policies can differ:
// everyone learns the first half equally well from a cold cache.
func DriftingZipfianTrace(seed int64, n, keySpace int, s float64) (trace Trace, postDrift []bool) {
	half := n / 2
	span := uint64(keySpace/2 - 1)
	r := rand.New(rand.NewSource(seed))
	z1 := rand.NewZipf(r, s, 1, span)
	z2 := rand.NewZipf(r, s, 1, span)
	trace = make(Trace, n)
	postDrift = make([]bool, n)
	for i := 0; i < half; i++ {
		trace[i] = int(z1.Uint64())
	}
	for i := half; i < n; i++ {
		trace[i] = keySpace/2 + int(z2.Uint64())
		postDrift[i] = true
	}
	return trace, postDrift
}
