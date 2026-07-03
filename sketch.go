package admit

// numProbes is the sketch depth: how many counters each key touches.
// Four is the standard choice in the TinyLFU literature and in Caffeine;
// past four the accuracy gain is marginal and the update cost isn't.
const numProbes = 4

// resetSampleFactor controls aging: the sketch halves every counter once
// total increments reach resetSampleFactor * counter count. Too low and
// frequency estimates never stabilize; too high and the sketch stops
// tracking shifts in the workload. 10x is Caffeine's default and there's
// no strong reason to deviate for a cache this size.
const resetSampleFactor = 10

// probeSeeds are odd 64-bit constants used to decorrelate the four probe
// positions from a single input hash, instead of running four separate
// hash functions. Each is a well-known avalanche multiplier (splitmix64 /
// murmur3 finalizer constants), chosen for good bit dispersion, not for
// any special relationship to each other.
var probeSeeds = [numProbes]uint64{
	0x9E3779B97F4A7C15,
	0xBF58476D1CE4E5B9,
	0x94D049BB133111EB,
	0xD6E8FEB86659FD93,
}

// sketch is a Count-Min Sketch over 4-bit saturating counters, packed 16
// to a uint64 word (0.5 bytes/counter instead of the 8 a plain int64
// slice would cost). It answers one question: "roughly how many times
// have we seen this key recently?" Collisions only ever inflate an
// estimate, never deflate it, which is the right failure mode for an
// admission filter, a false admit costs one wasted slot, a false reject
// costs nothing.
type sketch struct {
	table     []uint64
	mask      uint64
	additions int
	sampleMax int
}

// newSketch sizes the table for a cache holding capacity items. The word
// count (not the counter count) is rounded up to the next power of two
// at capacity, which is what Caffeine's FrequencySketch does: since each
// word packs 16 counters, that yields roughly 16x capacity counters
// total. That headroom matters, every Get and Set touches the sketch for
// its key regardless of whether the key is resident, so cardinality
// flowing through it is much larger than the cache itself; too few
// counters and unrelated keys collide constantly, and every estimate
// degrades toward noise.
func newSketch(capacity int) *sketch {
	words := 1
	for words < capacity {
		words <<= 1
	}
	if words < 1 {
		words = 1
	}
	return &sketch{
		table:     make([]uint64, words),
		mask:      uint64(words*16 - 1),
		sampleMax: resetSampleFactor * capacity,
	}
}

func (s *sketch) probe(h uint64, i int) uint64 {
	x := h ^ probeSeeds[i]
	x ^= x >> 33
	x *= 0xFF51AFD7ED558CCD
	x ^= x >> 33
	return x & s.mask
}

func counterLoc(idx uint64) (word int, shift uint64) {
	return int(idx >> 4), (idx & 15) * 4
}

// Add records one observation of the key with hash h, aging the whole
// table if enough observations have accumulated since the last reset.
func (s *sketch) Add(h uint64) {
	added := false
	for i := 0; i < numProbes; i++ {
		word, shift := counterLoc(s.probe(h, i))
		if v := (s.table[word] >> shift) & 0xF; v < 15 {
			s.table[word] += 1 << shift
			added = true
		}
	}
	if !added {
		return
	}
	s.additions++
	if s.additions >= s.sampleMax {
		s.reset()
	}
}

// Estimate returns the minimum count across the key's four counters,
// which Count-Min theory guarantees is >= the true observation count.
func (s *sketch) Estimate(h uint64) int {
	min := uint64(15)
	for i := 0; i < numProbes; i++ {
		word, shift := counterLoc(s.probe(h, i))
		if v := (s.table[word] >> shift) & 0xF; v < min {
			min = v
		}
	}
	return int(min)
}

// reset halves every counter in place. The mask strips the one bit that
// leaks across a nibble boundary when you right-shift a whole word at
// once, so each 4-bit counter ends up independently halved in a single
// pass instead of 16 separate shift-and-mask operations.
func (s *sketch) reset() {
	for i := range s.table {
		s.table[i] = (s.table[i] >> 1) & 0x7777777777777777
	}
	s.additions /= 2
}
