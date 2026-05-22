// Package metrics is a small, allocation-light, in-memory metrics store.
//
// Per-site we keep:
//   - lifetime totals (atomic counters)
//   - a per-second bucket ring for last 60s (req rate, bytes)
//   - a ring buffer of last 1024 latencies for percentile estimation
//   - status-code histogram
//
// Cost: ~6 KB per site. No goroutines, no allocations on the hot path.
package metrics

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	bucketCount = 60   // 60 one-second buckets → 1-minute window
	latRingSize = 1024 // last N latencies for percentile estimate
)

// Sample is one request observation.
type Sample struct {
	Status  int
	Bytes   int64
	Latency time.Duration
}

// Site holds counters for one routed domain.
type Site struct {
	// Atomic lifetime counters.
	TotalRequests atomic.Uint64
	TotalBytes    atomic.Uint64
	TotalErrors   atomic.Uint64 // 5xx + upstream-unreachable

	mu sync.Mutex
	// Per-second buckets.
	buckets [bucketCount]bucket
	// Index of the most recently written bucket.
	bucketAt int64
	// Latency ring.
	latency    [latRingSize]uint32 // milliseconds
	latencyIdx int
	latencyN   int
	// Status code distribution.
	statusCounts map[int]uint64
}

type bucket struct {
	tsUnix int64 // unix-second; zero = empty
	reqs   uint32
	bytes  uint64
	errors uint32
}

// NewSite returns a fresh Site metrics holder.
func NewSite() *Site {
	return &Site{statusCounts: map[int]uint64{}}
}

// Record observes one request.
func (s *Site) Record(sm Sample) {
	s.TotalRequests.Add(1)
	if sm.Bytes > 0 {
		s.TotalBytes.Add(uint64(sm.Bytes))
	}
	isErr := sm.Status >= 500 || sm.Status == 0
	if isErr {
		s.TotalErrors.Add(1)
	}

	nowSec := time.Now().Unix()
	ms := uint32(sm.Latency.Milliseconds())
	if ms == 0 && sm.Latency > 0 {
		ms = 1
	}

	s.mu.Lock()
	// Bucket: pick by (nowSec % bucketCount). If the slot's tsUnix is
	// stale (≠ nowSec), reset it — that's the cheap sliding window.
	idx := int(nowSec % bucketCount)
	b := &s.buckets[idx]
	if b.tsUnix != nowSec {
		*b = bucket{tsUnix: nowSec}
	}
	b.reqs++
	if sm.Bytes > 0 {
		b.bytes += uint64(sm.Bytes)
	}
	if isErr {
		b.errors++
	}
	s.bucketAt = nowSec

	// Latency ring.
	s.latency[s.latencyIdx] = ms
	s.latencyIdx = (s.latencyIdx + 1) % latRingSize
	if s.latencyN < latRingSize {
		s.latencyN++
	}

	// Status code.
	s.statusCounts[sm.Status]++
	s.mu.Unlock()
}

// Snapshot is a stable read-only view of one site's metrics.
type Snapshot struct {
	TotalRequests uint64
	TotalBytes    uint64
	TotalErrors   uint64
	// Last-60s aggregates.
	ReqsLast1m  uint64
	BytesLast1m uint64
	ErrsLast1m  uint64
	// Per-second series (oldest → newest).
	ReqsPerSec [bucketCount]uint32
	// Latency percentiles in ms (computed from the ring buffer).
	P50, P95, P99 uint32
	// Status code distribution.
	StatusCounts map[int]uint64
}

// Snapshot returns a copy safe for read across goroutines.
func (s *Site) Snapshot() Snapshot {
	out := Snapshot{
		TotalRequests: s.TotalRequests.Load(),
		TotalBytes:    s.TotalBytes.Load(),
		TotalErrors:   s.TotalErrors.Load(),
		StatusCounts:  map[int]uint64{},
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	nowSec := time.Now().Unix()
	for i := 0; i < bucketCount; i++ {
		b := s.buckets[i]
		// Only include buckets within the 60s window.
		if b.tsUnix == 0 || nowSec-b.tsUnix >= bucketCount {
			continue
		}
		out.ReqsLast1m += uint64(b.reqs)
		out.BytesLast1m += b.bytes
		out.ErrsLast1m += uint64(b.errors)
	}

	// Series ordered by absolute second so the UI can chart it.
	for offset := 0; offset < bucketCount; offset++ {
		ts := nowSec - int64(bucketCount-1-offset)
		idx := int(ts % bucketCount)
		b := s.buckets[idx]
		if b.tsUnix == ts {
			out.ReqsPerSec[offset] = b.reqs
		}
	}

	// Percentiles from latency ring.
	if s.latencyN > 0 {
		buf := make([]uint32, s.latencyN)
		copy(buf, s.latency[:s.latencyN])
		sort.Slice(buf, func(i, j int) bool { return buf[i] < buf[j] })
		out.P50 = pct(buf, 50)
		out.P95 = pct(buf, 95)
		out.P99 = pct(buf, 99)
	}

	for k, v := range s.statusCounts {
		out.StatusCounts[k] = v
	}
	return out
}

func pct(sorted []uint32, p int) uint32 {
	if len(sorted) == 0 {
		return 0
	}
	i := (len(sorted) * p) / 100
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// Registry holds per-domain Site stats with O(1) lookup.
type Registry struct {
	mu    sync.RWMutex
	sites map[string]*Site
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{sites: map[string]*Site{}}
}

// SiteFor returns (creating if needed) the Site stats for a domain.
func (r *Registry) SiteFor(domain string) *Site {
	r.mu.RLock()
	if s, ok := r.sites[domain]; ok {
		r.mu.RUnlock()
		return s
	}
	r.mu.RUnlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sites[domain]; ok {
		return s
	}
	s := NewSite()
	r.sites[domain] = s
	return s
}

// SnapshotAll returns snapshots of every tracked site, keyed by domain.
func (r *Registry) SnapshotAll() map[string]Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Snapshot, len(r.sites))
	for d, s := range r.sites {
		out[d] = s.Snapshot()
	}
	return out
}

// Prune removes domains no longer in the active list (used after a
// config reload that removes sites).
func (r *Registry) Prune(keep map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for d := range r.sites {
		if !keep[d] {
			delete(r.sites, d)
		}
	}
}
