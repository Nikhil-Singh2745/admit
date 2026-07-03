// Command bench replays synthetic traces through each eviction policy and
// prints hit ratios side by side. This is what generated the numbers in
// README.md.
package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"admit"
)

func identityHash(k int) uint64 { return uint64(k) }

type policy struct {
	name string
	new  func(capacity int) admit.Cache[int, int]
}

func policies() []policy {
	return []policy{
		{"LRU", func(cap int) admit.Cache[int, int] { return admit.NewLRU[int, int](cap) }},
		{"LFU", func(cap int) admit.Cache[int, int] { return admit.NewLFU[int, int](cap) }},
		{"W-TinyLFU", func(cap int) admit.Cache[int, int] { return admit.NewWTinyLFU[int, int](cap, identityHash) }},
	}
}

// simulate replays trace as a read-through cache: a miss is immediately
// followed by a Set, as if the value had just been fetched from whatever
// this cache sits in front of. Every entry is applied to the cache
// regardless of mask, but the hit ratio is computed only over entries
// where mask[i] is true (mask == nil counts everything). That distinction
// matters for workloads that mix cacheable and structurally-uncacheable
// accesses, see ScanPollutedTrace and DriftingZipfianTrace.
func simulate(c admit.Cache[int, int], trace admit.Trace, mask []bool) float64 {
	hits, opportunities := 0, 0
	for i, k := range trace {
		counts := mask == nil || mask[i]
		if _, ok := c.Get(k); ok {
			if counts {
				hits++
			}
		} else {
			c.Set(k, k)
		}
		if counts {
			opportunities++
		}
	}
	return 100 * float64(hits) / float64(opportunities)
}

type workload struct {
	name  string
	trace admit.Trace
	mask  []bool
}

func main() {
	const (
		keySpace = 10_000
		n        = 300_000
		seed     = 42
	)

	scanTrace, scanHot := admit.ScanPollutedTrace(seed, n, keySpace, 1.1, 2000, 500)
	driftTrace, driftMask := admit.DriftingZipfianTrace(seed, n, keySpace, 1.3)

	workloads := []workload{
		{"zipf s=1.1 (mild skew)", admit.ZipfianTrace(seed, n, keySpace, 1.1), nil},
		{"zipf s=1.5 (hot skew)", admit.ZipfianTrace(seed, n, keySpace, 1.5), nil},
		{"zipf s=1.1 + scans (hot-key hit%)", scanTrace, scanHot},
		{"zipf s=1.3, hot set flips at 50% (post-flip hit%)", driftTrace, driftMask},
	}
	sizePercents := []int{1, 5, 10, 20}

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "workload\tcache\tLRU\tLFU\tW-TinyLFU")
	for _, wl := range workloads {
		for _, pct := range sizePercents {
			capacity := keySpace * pct / 100
			if capacity < 1 {
				capacity = 1
			}
			fmt.Fprintf(w, "%s\t%d%%\t", wl.name, pct)
			for i, p := range policies() {
				hr := simulate(p.new(capacity), wl.trace, wl.mask)
				if i > 0 {
					fmt.Fprint(w, "\t")
				}
				fmt.Fprintf(w, "%5.2f%%", hr)
			}
			fmt.Fprintln(w)
		}
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
