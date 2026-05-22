package proxy

import (
	"net/url"
	"strings"
	"sync/atomic"
)

// Pool holds the upstreams for a site and round-robins across them.
// Health-check failures don't permanently eject — the next request just
// rolls the dice on the next upstream. For workloads of this scale
// (personal VPS) that's a fine tradeoff against the complexity of an
// always-on health-checker goroutine per site.
type Pool struct {
	upstreams []*url.URL
	next      atomic.Uint64
}

// NewPool parses each upstream string into a *url.URL. Bare host:port
// is treated as http:// for ergonomics.
func NewPool(upstreams []string) (*Pool, error) {
	urls := make([]*url.URL, 0, len(upstreams))
	for _, raw := range upstreams {
		u, err := parseUpstream(raw)
		if err != nil {
			return nil, err
		}
		urls = append(urls, u)
	}
	return &Pool{upstreams: urls}, nil
}

func parseUpstream(raw string) (*url.URL, error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return url.Parse(raw)
}

// Next picks the next upstream URL. Caller must not mutate the result.
func (p *Pool) Next() *url.URL {
	if len(p.upstreams) == 0 {
		return nil
	}
	n := p.next.Add(1) - 1
	return p.upstreams[n%uint64(len(p.upstreams))]
}

// Size returns the count of upstreams in this pool.
func (p *Pool) Size() int { return len(p.upstreams) }
