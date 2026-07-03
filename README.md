# admit

An admission-controlled cache in Go: LRU, LFU, and W-TinyLFU implemented from scratch,
benchmarked against each other on synthetic traces designed to expose their specific
failure modes. Zero dependencies, stdlib only.

This is written as a research note, not a library pitch. The question under test is
narrow: does frequency-based admission control (W-TinyLFU) actually beat plain LRU, and
does it pay for that with LFU's known weakness (staleness)? The answer is in the Results
section, backed by numbers you can regenerate with one command, not asserted from
memory.

## Abstract

LRU is the default eviction policy almost everywhere, and it has a specific, well-known
failure mode: it is purely recency-based, so it cannot distinguish a key that is
genuinely hot from a key that was merely touched once, recently. LFU fixes that by
tracking real access frequency, but naive LFU has the opposite problem: it never
forgets, so a key's historical popularity outlives its actual relevance and the cache
stops adapting.

W-TinyLFU (Einziger, Friedman, Manes, 2015) resolves this by decoupling *admission* from
*eviction*. Eviction is still handled by ordinary LRU-family logic, a Segmented LRU with
a probation and protected tier. Admission is gated separately, by an approximate,
self-aging frequency estimate (a Count-Min Sketch) that decides whether a newly-evicted
window candidate deserves to displace an existing main-cache entry at all. This is the
policy both Caffeine (Java) and Ristretto (Go) ship as their production default, not an
academic curiosity.

This repo implements all three policies under one interface, generates three
purpose-built synthetic workloads (stationary skew, scan pollution, concept drift), and
measures hit ratio and throughput for each. Result: W-TinyLFU matches or beats LFU on
every workload tested and never loses badly anywhere, which LRU and LFU each do on at
least one workload. The cost is roughly 30% more time per operation than LRU, entirely
attributable to the sketch probes.

## 1. Background

A cache eviction policy answers one question: when the cache is full and a new key
arrives, what leaves? The naive answer, evict whatever hasn't been touched in the
longest time, is LRU, and it is nearly universal because it's simple, O(1), and correct
enough for uniformly random access patterns.

It stops being correct enough the moment access patterns aren't uniform, which is almost
always. Two specific failure modes matter here:

**Scan pollution.** A single pass over cold data, a batch export, a backfill job, a
crawler walking every row once, pushes every genuinely hot key out of an LRU cache,
because every key touched during the scan is, by definition, the most recently used key
at the moment it's touched. The cache's entire state gets overwritten by data that will
never be read again, and the actual working set has to be rebuilt from nothing
afterward.

**Frequency blindness.** LRU has no concept of "how often," only "how recent." A key
read 10,000 times an hour and a key read once, thirty seconds ago, look identical to
LRU: both are at the front of the list. There is no mechanism by which sustained
popularity earns any protection.

LFU is the direct fix: track actual access counts, evict the least-frequently-used key.
It's immune to both problems above by construction, a scanned key has frequency 1 and
is evicted almost immediately; a genuinely hot key accumulates a count that makes it
essentially permanent. But this immunity is exactly the new problem: "essentially
permanent" doesn't distinguish *was* hot from *is* hot. A key that had a frequency count
of 10,000 last month, before traffic patterns shifted, still has a frequency count of
10,000 today, and a brand new key that just became genuinely hot has to out-count a
number that will never go down. Naive LFU has no forgetting mechanism, and it cold-starts
new keys catastrophically badly, which is a second, related problem: frequency 1 loses
to frequency 10,000 even on the new key's best day.

### 1.1 Prior approaches

The systems literature has circled this tradeoff for decades before TinyLFU. **ARC**
(Megiddo & Modha, 2003) adapts between recency and frequency by tracking two LRU lists
plus two "ghost" lists of recently evicted keys, and shifts the balance based on ghost
list hit rate. **2Q** (Johnson & Shasha, 1994) is a simpler two-queue version of the
same idea, a short FIFO for first-touch keys, a longer LRU for keys that get touched
twice. Both work, and both require you to track metadata for keys that have already
been evicted, which costs memory proportional to eviction traffic, not cache size.

TinyLFU's move is to replace ghost lists with a compact, approximate frequency sketch.
Instead of remembering *which* keys were evicted, it remembers *how often keys were seen*,
in a fixed, small amount of memory that doesn't grow with traffic. That's the actual
research contribution being tested here: is an approximate, bounded-memory frequency
estimate good enough to replace exact-but-unbounded ghost-list bookkeeping? On these
workloads, yes.

## 2. Hypothesis

Three claims, each falsifiable by the benchmark harness in this repo:

1. On a stationary Zipfian workload (no drift, no pollution), W-TinyLFU's hit ratio
   converges to LFU's, both above LRU. A correctly-tuned frequency filter should behave
   like frequency tracking when frequency doesn't change.
2. On a scan-polluted workload, W-TinyLFU's hit ratio on the actual (non-scan) working
   set stays close to LFU's and well above LRU's. The admission filter should reject
   one-hit-wonders regardless of how recent they are.
3. On a workload where the hot key set changes partway through (concept drift),
   W-TinyLFU recovers close to LRU's adaptability, while naive LFU collapses, because
   its frequency counts from before the drift never decay.

All three are tested directly in Section 6, not assumed.

## 3. Architecture

```
cache.go          Cache[K,V] interface, shared node type, intrusive list
sketch.go          Count-Min Sketch: packed 4-bit counters, aging
lru.go             baseline: LRU
lfu.go             baseline: LFU (frequency buckets)
wtinylfu.go        window admission + Segmented LRU main cache
hash.go            HashString, a ready-made hash for string keys
trace.go           synthetic workload generators
cache_test.go      behavioral tests + throughput benchmarks
sketch_test.go     sketch correctness tests
cmd/bench/main.go  hit-ratio harness, produces the table in Section 6
```

### 3.1 The shared interface

```go
type Cache[K comparable, V any] interface {
    Get(key K) (V, bool)
    Set(key K, value V)
    Len() int
}
```

All three policies implement this and share one generic `node[K, V]` and one intrusive
doubly linked list (`cache.go`), rather than each hand-rolling its own list. `node`
carries a `freq int` field used only by LFU and a `seg uint8` field used only by
W-TinyLFU; LRU ignores both. That's dead weight for LRU specifically, roughly 9 bytes
per entry it doesn't need, and it's a deliberate trade: one list implementation with a
little waste beats three near-identical ones with none. The list uses sentinel head/tail
nodes so push and remove never special-case an empty list.

Every operation across all three policies is O(1) amortized. W-TinyLFU adds a constant
number of sketch probes (4, fixed) on top, discussed in 3.2.

### 3.2 Count-Min Sketch (`sketch.go`)

The frequency estimator behind admission. A Count-Min Sketch answers "roughly how many
times have I seen this key" in fixed memory, trading exactness for boundedness: it can
overestimate a key's frequency (via hash collisions with other keys) but never
underestimate it. That asymmetry is the right one for this use case. A false admit costs
one wasted cache slot until it's evicted again. A false reject costs a hit that could
have been retained. Overestimation is the survivable failure mode; underestimation would
starve real hot keys, which is much worse.

**Layout.** Counters are 4 bits wide, saturating at 15, packed 16 to a `uint64` word.
That's 0.5 bytes per counter versus 8 bytes for a naive `int64` slice, a 16x memory
reduction, matching Caffeine's actual `FrequencySketch` layout rather than a
back-of-envelope guess. Four bits is enough: aging (below) keeps counts from ever
needing to represent more than roughly 10x the reset threshold's worth of unaged
observations, and saturation just means "very hot," which is all the admission decision
needs to know.

**Probing.** Each key gets 4 counter positions (`numProbes = 4`, the standard depth in
the TinyLFU literature: enough to keep false-positive collision rate low, more than that
has a fast-diminishing accuracy return for a linear cost increase). All 4 positions are
derived from a *single* 64-bit hash of the key, each XORed against a different
avalanche-mix constant (`probeSeeds`, splitmix64/murmur3 finalizer constants chosen for
bit dispersion, not any special relationship between them) and run through one
multiply-xor-shift mix. This is a deliberate choice over computing four independent
hashes: one cheap mix per probe is close to free, three *additional full hash
computations* are not, and empirically the mixed-single-hash approach gives probe
positions that are independent enough for Count-Min's guarantees to hold in practice.

**Sizing.** `newSketch(capacity)` rounds the *word count* up to the next power of two at
or above `capacity`, not the counter count. Since each word holds 16 counters, this
gives roughly 16x `capacity` counters total for a cache of size `capacity`. That
headroom is not cosmetic: every `Get` and every `Set` touches the sketch for its key
*whether or not that key is resident in the cache*, so the cardinality flowing through
the sketch over time is much larger than the cache's own capacity. A table sized 1:1
with capacity collapses into collision noise almost immediately under any real load,
because far more distinct keys pass through the sketch than the cache ever holds at
once.

This is worth calling out honestly because it's exactly the bug that showed up during
development: the first version of `newSketch` sized the table to `capacity` in raw
counters (roughly a 16x undersizing versus the current version), and under that bug
W-TinyLFU could not reliably beat plain LRU on the scan-pollution workload, its
admission filter had degraded into near-random noise. Fixing the sizing to match
Caffeine's approach is what makes the numbers in Section 6 hold up. Getting sketch
sizing wrong is a silent failure, nothing crashes, nothing errors, the cache just quietly
stops discriminating between hot and cold keys, and the only way to catch it is to
measure hit ratio and notice it's wrong.

**Aging.** Every `resetSampleFactor * capacity` additions (`resetSampleFactor = 10`,
Caffeine's default, no strong reason found to deviate for a cache this size), the entire
table is halved in place:

```go
s.table[i] = (s.table[i] >> 1) & 0x7777777777777777
```

A naive halving would right-shift each 4-bit counter independently, 16 masked
shift-and-OR operations per word. This does all 16 in one shift and one mask: shifting
the whole 64-bit word right by one bit correctly halves each nibble's value, except that
the top bit of each nibble leaks one bit into the bottom of the nibble above it, which
the `0x7777...` mask (0111 repeated) strips back out. One shift, one AND, sixteen
counters halved. This is the mechanism that makes the sketch track *recent* frequency
rather than *all-time* frequency, and it's precisely the capability naive LFU is missing.
Without aging, this sketch would just be a memory-efficient LFU, with the same
staleness problem.

### 3.3 W-TinyLFU (`wtinylfu.go`)

The full policy composes three structures:

- A **window**, plain LRU, sized to `windowPercent` (1%) of total capacity. All new keys
  enter here first, unconditionally, no admission check.
- A **main cache**, a Segmented LRU split into **probation** and **protected** tiers,
  `protectedPercent` (80%) of main capacity reserved for protected. This is Caffeine's
  SLRU split, not an arbitrary choice: the second access is what promotes a key from
  probation to protected, a hard threshold rather than a gradual one, which keeps the
  promotion rule cheap to reason about and cheap to implement (no decay curve, no
  score, just "have I seen this key more than once").
- The **sketch** from 3.2, which gates the one moment that actually matters: when a key
  leaves the window and wants into main.

**Write path.** A new key always enters the window at the front (`Set`, when the key
isn't already present). If that pushes the window over its capacity, the window's LRU
victim becomes an admission *candidate*, handled by `admitToMain`. If main isn't yet at
capacity (the cache overall is still warming up), the candidate is admitted for free,
no comparison needed, there's nothing to evict yet. Once main is full, the candidate has
to win a frequency comparison against main's current probation-tier victim (falling back
to protected's victim if probation happens to be empty): whichever of the two has the
higher sketch estimate survives, and ties favor the incumbent. Losing candidates are
simply dropped, not retried, not queued: a rejected key gets another shot the next time
it's accessed, same as any other cold key.

**Read path.** Every `Get`, hit or miss, records an observation in the sketch
(`c.sk.Add`) before the lookup. This matters: frequency has to be tracked for keys that
*aren't* resident too, otherwise a key could never accumulate enough estimated frequency
to win admission in the first place, since it would only be counted while it happened to
already be in the cache. On a hit, `access(n)` moves the node within its current segment
(window and protected both just move-to-front on any touch) or promotes it from
probation to protected on its second access, demoting protected's coldest entry back to
probation if that promotion pushes protected over its 80% share.

Favoring the incumbent on ties is deliberate. Without it, two keys with genuinely equal,
noisy sketch estimates would flip-flop in and out of main on essentially random
collision noise, adding churn without adding hit ratio. Requiring a strict win to evict
gives the cache hysteresis for free.

### 3.4 Baselines (`lru.go`, `lfu.go`)

Both exist so the comparison in Section 6 isn't against a strawman. LRU is a standard
map-plus-intrusive-list implementation, no shortcuts. LFU buckets nodes by current
frequency count in a `map[int]*list[K,V]`, tracks `minFreq` to know which bucket to
evict from next, and promotes a node one bucket at a time on each access, advancing
`minFreq` past any bucket that promotion just emptied. Every operation in both is O(1)
amortized, same complexity class as W-TinyLFU minus the sketch probes.

## 4. Methodology

### 4.1 Workload generation (`trace.go`)

Real production traces would be the gold standard here, but they're not available
outside a production system, and hand-picked "realistic-looking" traces are a good way
to smuggle in a foregone conclusion. Instead, each workload targets exactly one
hypothesis from Section 2, deterministically, so the result is reproducible and the
reasoning behind the workload's shape is explicit rather than implied.

**Stationary Zipfian** (`ZipfianTrace`). Built on `math/rand`'s Zipf generator (Gray et
al.'s algorithm), not a hand-rolled one, there's no reason to reimplement a distribution
the stdlib already gets right. Skew parameter `s` controls how concentrated traffic is
on a small key subset; `s=1.1` (mild) and `s=1.5` (hot) are both tested, since a policy
that only wins under one skew level isn't actually validated.

**Scan-polluted** (`ScanPollutedTrace`). Interleaves the Zipfian hot traffic above with
periodic bursts of sequential, never-repeated cold keys drawn from outside the hot key
space, simulating a batch scan hitting a cache that's also serving real traffic. The
function returns a `hot []bool` mask alongside the trace. This mask matters
methodologically: the cold scan keys are guaranteed misses for *every* policy by
construction (they're never repeated), so if you compute hit ratio over the raw trace,
you dilute every policy's number by the same constant and the actual differential
damage LRU suffers gets hidden inside noise that has nothing to do with eviction policy.
Hit ratio is measured only over `hot[i] == true` entries, i.e. only over accesses that
were structurally capable of being a hit.

**Concept drift** (`DriftingZipfianTrace`). The first half of the trace is Zipfian over
one key range; the second half is Zipfian over a disjoint range of the same size, an
instantaneous, total shift in what's hot. This workload was added specifically because
the first two don't surface LFU's core weakness: plain LFU is nearly immune to one-time
scan pollution (a scanned key gets frequency 1 and is evicted almost immediately
regardless of policy), so scan pollution alone makes LFU look strictly better than LRU
with no visible downside. Concept drift is the workload where LFU's lack of a forgetting
mechanism actually costs it. `postDrift` marks the second-half entries; hit ratio is
measured only there, since every policy learns the first half equally well starting from
an empty cache, and the interesting question is entirely about the second half.

All three generators take an explicit `seed`, and the benchmark harness fixes
`seed = 42`, `keySpace = 10,000`, `n = 300,000` for every workload. Nothing in Section 6
is order-dependent or run-to-run noisy; rerunning `go run ./cmd/bench` reproduces the
exact same table.

### 4.2 Simulation harness (`cmd/bench/main.go`)

`simulate` replays a trace as a read-through cache: a miss is immediately followed by a
`Set`, as if the value had just been fetched from whatever this cache is fronting. Every
trace entry is applied to the cache regardless of the mask (the cache's internal state
has to reflect the full trace, including cold scan keys, for the scan-pollution
scenario to mean anything), but the hit-ratio numerator and denominator only accumulate
over entries where `mask[i]` is true, per the reasoning in 4.1. `mask == nil` counts
everything, used for the two plain Zipfian workloads where there's no structural
uncacheable-key issue to correct for.

Cache size is expressed as a percentage of key space (1%, 5%, 10%, 20%) rather than an
absolute number, since the interesting variable is how tightly memory-constrained the
cache is relative to the workload's actual key cardinality, not the raw entry count.

## 5. Complexity and memory

| Operation | LRU | LFU | W-TinyLFU |
|---|---|---|---|
| `Get` (hit) | O(1) | O(1) amortized | O(1) amortized + 4 sketch probes |
| `Get` (miss) | O(1) | O(1) | O(1) + 4 sketch probes (frequency still recorded) |
| `Set` (new key, room available) | O(1) | O(1) | O(1) + 4 sketch probes |
| `Set` (new key, eviction) | O(1) | O(1) | O(1) amortized + up to 8 sketch probes (candidate and victim) |
| Sketch reset | n/a | n/a | O(capacity), amortized O(1) per addition (fires once per `10 * capacity` additions) |

Per-entry memory overhead beyond `K` and `V` is one `node[K,V]`: two pointers (16 bytes),
one `freq int` (8 bytes, LFU-only), one `seg uint8` (1 byte, W-TinyLFU-only), plus struct
padding. `BenchmarkLRU`/`LFU`/`WTinyLFU` all report 48 B/op, 1 alloc/op for
`int, int` entries, which matches: one `node[int,int]` allocation per new key, no
incidental allocation in the hot path of any policy. The sketch adds a fixed
`words * 8` bytes independent of how many keys are ever seen, roughly `capacity` bytes
after rounding to a power of two, which is the entire point of using an approximate
structure instead of an exact per-key counter map.

## 6. Results

Generated by `go run ./cmd/bench`. Zipfian trace, key space 10,000, 300,000 accesses,
seed 42, fully deterministic.

```
workload                                           cache  LRU     LFU     W-TinyLFU
zipf s=1.1 (mild skew)                             1%     52.89%  61.88%  62.39%
zipf s=1.1 (mild skew)                             5%     70.88%  76.72%  76.95%
zipf s=1.1 (mild skew)                             10%    77.97%  82.09%  82.52%
zipf s=1.1 (mild skew)                             20%    84.73%  87.30%  87.55%
zipf s=1.5 (hot skew)                              1%     89.82%  92.32%  92.23%
zipf s=1.5 (hot skew)                              5%     95.97%  96.83%  96.84%
zipf s=1.5 (hot skew)                              10%    97.39%  97.77%  97.82%
zipf s=1.5 (hot skew)                              20%    98.29%  98.36%  98.38%
zipf s=1.1 + scans (hot-key hit%)                  1%     51.94%  61.80%  62.23%
zipf s=1.1 + scans (hot-key hit%)                  5%     65.03%  76.53%  76.45%
zipf s=1.1 + scans (hot-key hit%)                  10%    71.39%  81.71%  81.93%
zipf s=1.1 + scans (hot-key hit%)                  20%    78.13%  85.86%  86.65%
zipf s=1.3, hot set flips at 50% (post-flip hit%)  1%     77.70%  28.58%  82.67%
zipf s=1.3, hot set flips at 50% (post-flip hit%)  5%     89.73%  53.28%  91.29%
zipf s=1.3, hot set flips at 50% (post-flip hit%)  10%    93.35%  78.21%  93.26%
zipf s=1.3, hot set flips at 50% (post-flip hit%)  20%    96.10%  87.11%  94.07%
```

Throughput, `go test -bench . -benchmem` (i3-1315U):

```
BenchmarkLRU-8        9,853,168   120.2 ns/op   48 B/op   1 allocs/op
BenchmarkLFU-8        9,228,642   129.8 ns/op   48 B/op   1 allocs/op
BenchmarkWTinyLFU-8   7,329,252   157.5 ns/op   48 B/op   1 allocs/op
```

### 6.1 Reading the table

**Stationary Zipfian**, both skews: W-TinyLFU tracks LFU almost exactly, both clearly
ahead of LRU, and the gap between LFU and W-TinyLFU is within noise at every cache size.
This confirms hypothesis 1 directly. It's also the least interesting result on its own,
a correctly-behaving frequency filter *should* converge to frequency tracking when the
underlying frequency distribution never moves. It's the necessary baseline for the two
workloads that follow, not the headline.

**Scan-polluted**, hit ratio measured only over the repeatable hot keys per Section 4.1:
LRU loses 5 to 10 points of hit ratio to the scan, worse at smaller cache sizes where
the scan represents a proportionally larger fraction of what gets evicted. LFU and
W-TinyLFU barely register the scan at all, both staying within a point of their
scan-free numbers. Confirms hypothesis 2. This is the clean, textbook case for
admission control: a workload that is entirely about rejecting one-hit-wonders, and both
frequency-aware policies reject them essentially for free.

**Concept drift**, hit ratio measured only on the post-flip half: this is where the
three policies actually diverge, and where the case for W-TinyLFU over plain LFU
gets made. LFU is worse than LRU at *every* cache size tested, and catastrophically
worse at small ones: 28.58% versus LRU's 77.70% at 1% capacity. Its frequency counts
from the first half of the trace never decay, so after the drift it's still actively
defending a working set that no longer exists, actively refusing to admit the keys
that are now actually hot because they can't out-count stale history. W-TinyLFU's
aging sketch recovers to within a few points of LRU at small cache sizes and
essentially matches LRU by 10% capacity, because the sketch's counts from before the
drift have decayed enough by the time the drift happens that new keys can compete on
close to even footing.

Notably, LRU is the *best* policy on this one workload, and by a real margin at 20%
capacity (96.10% versus W-TinyLFU's 94.07%). LRU has no memory of anything beyond
strict recency, so it adapts to a total distribution shift the fastest of the three,
having nothing old to unlearn. That's not a weakness in the argument for admission
control, it's the reason W-TinyLFU keeps a recency window at all instead of routing
100% of admission decisions through the frequency filter: the window is what gives it
a fast path back to LRU-like behavior when the sketch's aged history stops being
predictive.

### 6.2 The actual conclusion

W-TinyLFU is the only one of the three policies that doesn't have a workload in this
suite where it loses badly. LRU loses badly to scan pollution. LFU loses badly, worse
than either alternative, to concept drift. W-TinyLFU's worst showing anywhere is a few
points behind LRU on drift recovery at large cache sizes, never behind LFU by more than
noise on any stationary workload. That absence of a catastrophic failure mode, not raw
speed (it's the slowest of the three, see below), is the actual argument for admission
control in production. Caffeine and Ristretto did not choose W-TinyLFU because it's
fast. They chose it because it doesn't have a workload that breaks it.

The throughput cost is real and worth stating plainly: W-TinyLFU runs about 30% slower
per operation than LRU on this hardware, and the entire delta is the sketch probes (4
mixes per `Get`, up to 8 more per admission decision on eviction). That's the trade
being made: worse constant factor, no catastrophic failure mode, versus LRU's better
constant factor with a known, exploitable weakness.

## 7. Threats to validity

Being direct about what this benchmark does and doesn't prove:

- **Synthetic traces, not production traces.** Zipfian distributions are a standard,
  well-justified model for cache access patterns (this is the same distribution family
  used in the original ARC and TinyLFU papers' evaluations), but they are still a model.
  A real production trace has correlated key co-access, time-of-day effects, and
  workload-specific structure that a synthetic Zipfian generator does not reproduce.
  The scan-pollution and concept-drift workloads are constructed specifically to isolate
  one failure mode each, which is good for attributing *why* a policy wins or loses, but
  means the numbers here are a controlled demonstration, not a production capacity
  forecast.
- **Single-threaded.** All benchmarks run one goroutine at a time. Real cache load is
  concurrent, and a mutex or sharding layer would change the throughput numbers in
  Section 6.2, though it would not change the hit-ratio numbers in Section 6.1, since
  eviction and admission decisions are logically the same regardless of how they're
  synchronized.
- **One hardware target.** Throughput numbers are from one machine (i3-1315U). Absolute
  ns/op will differ elsewhere; the relative ordering (LRU fastest, W-TinyLFU slowest by
  the sketch overhead) should hold generally, since it follows directly from operation
  count, not from anything hardware-specific.
- **Fixed window and segment sizes.** `windowPercent` (1%) and `protectedPercent` (80%)
  are fixed constants here, matching Caffeine's defaults. Caffeine's actual production
  implementation adaptively resizes the window at runtime via hill-climbing on
  observed hit rate; this implementation does not, on the reasoning that adaptive
  sizing is a refinement on top of a working admission filter, not a precondition for
  one, and fixed 1%/80% performs well across every skew level tested here. A workload
  whose optimal window size is far from 1% would show W-TinyLFU underperforming its own
  ceiling in this implementation specifically, not a flaw in the admission-control idea
  itself.

## 8. Usage

```go
c := admit.NewWTinyLFU[string, []byte](10_000, admit.HashString)

c.Set("key", []byte("value"))
v, ok := c.Get("key")
```

Keys are hashed by a function you supply, not by reflection. `HashString` covers string
keys (`hash/maphash`, process-seeded, don't persist or compare hashes across runs). For
any other key type, write a `func(K) uint64`, an identity cast is fine for integer keys,
`Estimate`'s probes already do the avalanche mixing internally so the input hash doesn't
need to be high quality on its own.

## 9. Reproducing

```
go build ./...
go vet ./...
go test ./...
go test -bench . -benchmem
go run ./cmd/bench
```

Go 1.22+, no other setup, no network access required.

## 10. Limitations and future work

- **Not concurrent.** No mutex, no sharding, no read-buffer batching. Wrapping this in a
  `sync.Mutex` would work and wouldn't change any hit-ratio number in Section 6, since
  concurrency is orthogonal to eviction policy; it would have buried the part of this
  project that's actually about eviction policy under boilerplate that's been solved a
  thousand times elsewhere.
- **No TTL, no size-weighted eviction.** Every entry counts as exactly one unit of
  capacity regardless of its actual size in bytes. A real cache fronting
  variable-size values would want weighted capacity accounting.
- **No adaptive window sizing.** See Section 7. Fixed 1%/80% split, not Caffeine's
  hill-climbing adaptive window.
- **No doorkeeper.** The original TinyLFU paper describes an optional Bloom filter
  gate in front of the sketch, so a key's very first observation doesn't cost a sketch
  write, only its second does. This is a constant-factor optimization on top of an
  already-correct admission decision, not something the admission logic depends on, and
  it wasn't implemented here for that reason.
- **Frequency is approximate by construction.** That's the entire point of using a
  sketch instead of an exact counter map, but it means admission decisions near a tie
  are inherently noisy. Ties favor the incumbent, which trades a small amount of
  responsiveness for a large amount of stability.
- **Explicit hash function, not reflection.** Deliberate: no `unsafe`, no `reflect`, and
  no silently bad default hash for a key type this package was never tested against.

## References

- Einziger, G., Friedman, R., Manes, N. "TinyLFU: A Highly Efficient Cache Admission
  Policy." 2015.
- Megiddo, N., Modha, D. S. "ARC: A Self-Tuning, Low Overhead Replacement Cache." FAST
  2003.
- Johnson, T., Shasha, D. "2Q: A Low Overhead High Performance Buffer Management
  Replacement Algorithm." VLDB 1994.
- Cormode, G., Muthukrishnan, S. "An Improved Data Stream Summary: The Count-Min
  Sketch and its Applications." 2005.
- Caffeine (Java caching library), `com.github.benmanes.caffeine`, whose
  `FrequencySketch` sizing and SLRU split this implementation follows directly.
- Ristretto (Go caching library), `github.com/dgraph-io/ristretto`, the other major
  production implementation of the same policy.
