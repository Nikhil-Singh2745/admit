package admit

import "testing"

func TestSketchEstimateNeverUnderestimates(t *testing.T) {
	s := newSketch(1024)
	const key, adds = uint64(42), 7
	for i := 0; i < adds; i++ {
		s.Add(key)
	}
	if got := s.Estimate(key); got < adds {
		t.Fatalf("Estimate(%d adds) = %d, want >= %d", adds, got, adds)
	}
}

func TestSketchSaturatesAtFifteen(t *testing.T) {
	s := newSketch(1024)
	const key = uint64(7)
	for i := 0; i < 100; i++ {
		s.Add(key)
	}
	if got := s.Estimate(key); got != 15 {
		t.Fatalf("Estimate after saturation = %d, want 15", got)
	}
}

func TestSketchDistinctKeysDontInflateEachOther(t *testing.T) {
	s := newSketch(4096)
	for i := 0; i < 200; i++ {
		s.Add(uint64(i))
	}
	if got := s.Estimate(999999); got > 2 {
		t.Fatalf("Estimate(never-seen key) = %d, want a small number (some collision noise is expected)", got)
	}
}

func TestSketchAgingHalvesCounts(t *testing.T) {
	s := newSketch(16)
	const key = uint64(1)
	for i := 0; i < 5; i++ {
		s.Add(key)
	}
	before := s.Estimate(key)

	// Pad with distinct keys, not a single repeated one, so collision
	// noise spreads thin across the table instead of stacking onto one
	// set of probe slots. additions resets itself (halves) the instant it
	// reaches sampleMax, so it never settles above the threshold - loop a
	// fixed number of times guaranteed to cross it at least once instead
	// of waiting on additions directly.
	for i := uint64(0); i < uint64(s.sampleMax)+10; i++ {
		s.Add(1_000_000 + i)
	}
	after := s.Estimate(key)

	if after > before {
		t.Fatalf("Estimate after reset = %d, want <= %d (aging must not raise counts)", after, before)
	}
	if after < before/2-1 || after > before/2+1 {
		t.Fatalf("Estimate after reset = %d, want roughly %d (half of %d)", after, before/2, before)
	}
}
