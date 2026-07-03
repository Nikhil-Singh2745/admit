package admit

import "hash/maphash"

var hashSeed = maphash.MakeSeed()

// HashString is a ready-made hash function for string keys, for passing
// to NewWTinyLFU. It's seeded once per process, so don't persist these
// hashes or compare them across runs.
func HashString(s string) uint64 {
	return maphash.String(hashSeed, s)
}
