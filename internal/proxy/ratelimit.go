package proxy

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tokenBucket is a per-key leaky bucket with floating-point fill.
type tokenBucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a per-(site, source-IP) token-bucket store.
//
// Layout: one Limiter per Site (so configurations don't bleed across
// sites). Keys are stringified source IPs. The implementation is
// intentionally simple — no LRU eviction; the map grows with unique
// client IPs. For a personal VPS where the IP set is bounded by
// real-world traffic, this is fine. For abuse-grade DDoS protection,
// upstream that responsibility to Cloudflare / your firewall.
type Limiter struct {
	rps   float64
	burst float64
	mu    sync.Mutex
	bs    map[string]*tokenBucket
}

// NewLimiter returns a limiter with the given sustained rate (rps) and
// burst capacity.
func NewLimiter(rps, burst int) *Limiter {
	if rps <= 0 {
		return nil // disabled
	}
	if burst < 0 {
		burst = 0
	}
	return &Limiter{
		rps:   float64(rps),
		burst: float64(rps + burst),
		bs:    map[string]*tokenBucket{},
	}
}

// Allow returns true if the request from `key` is within the budget.
func (l *Limiter) Allow(key string) bool {
	if l == nil {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	tb, ok := l.bs[key]
	if !ok {
		// First request from this IP starts with a full burst budget.
		l.bs[key] = &tokenBucket{tokens: l.burst - 1, last: now}
		return true
	}
	// Refill since last visit.
	elapsed := now.Sub(tb.last).Seconds()
	tb.tokens += elapsed * l.rps
	if tb.tokens > l.burst {
		tb.tokens = l.burst
	}
	tb.last = now
	if tb.tokens < 1 {
		return false
	}
	tb.tokens--
	return true
}

// ClientIP returns the best-effort source IP for `r`. If
// trustForwardedHeaders is true, the first hop in X-Forwarded-For is
// used; otherwise the direct peer is used (the safer default).
func ClientIP(r *http.Request, trustForwardedHeaders bool) string {
	if trustForwardedHeaders {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
		if xri := r.Header.Get("X-Real-IP"); xri != "" {
			return strings.TrimSpace(xri)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
