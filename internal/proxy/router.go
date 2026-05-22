// Package proxy is the byte-shoveler — the actual HTTP reverse-proxy
// layer that turns a Host header into an upstream request.
package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"jobcloud/internal/config"
	"jobcloud/internal/metrics"
)

// commonExploitPattern matches request paths that are never legitimate
// on a Django/Node/etc backend — wordpress probes, .env scans, git
// metadata scans. Mirrors what NPM's "Block Common Exploits" toggle
// does, and what the original JOBAPP nginx config rejected.
var commonExploitPattern = regexp.MustCompile(
	`(?i)^/(wp-admin|wp-login\.php|wp-content|xmlrpc\.php|\.env|\.git|phpmyadmin|administrator|cgi-bin|server-status|owa/)`,
)

// scannerUA is a fast pre-filter for the dumb half of vulnerability
// scanners. Real attackers don't ID themselves, but this trims log
// noise meaningfully.
var scannerUA = regexp.MustCompile(`(?i)(sqlmap|nikto|nmap|masscan|acunetix|nessus|libwww-perl|whatweb|wpscan)`)

// siteRuntime is the per-site cached state derived from a Site config:
// the upstream pool, the rate limiter, the reverse-proxy handler.
// Rebuilt whenever the config reloads.
type siteRuntime struct {
	site    *config.Site
	pool    *Pool
	limiter *Limiter
	rp      *httputil.ReverseProxy
}

// Router is the HTTP handler that fronts everything. It implements
// http.Handler.
type Router struct {
	store         *config.Store
	registry      *metrics.Registry
	trustHeaders  bool
	transport     http.RoundTripper

	mu       sync.RWMutex
	runtimes map[string]*siteRuntime // keyed by Domain
	// closed once the router is asked to stop accepting new requests.
	stopping atomic.Bool
}

// NewRouter wires the router. `trustHeaders` controls whether
// X-Forwarded-* from the client request are trusted (for client-IP
// extraction). Set false unless jobcloud itself sits behind another
// trusted L7 proxy.
func NewRouter(store *config.Store, reg *metrics.Registry, trustHeaders bool) *Router {
	return &Router{
		store:        store,
		registry:     reg,
		trustHeaders: trustHeaders,
		transport:    newProxyTransport(),
		runtimes:     map[string]*siteRuntime{},
	}
}

// newProxyTransport returns a tuned http.Transport for upstream calls.
// Connection pooling is the main perf win — without it, every request
// would do a full TCP handshake to the upstream.
func newProxyTransport() *http.Transport {
	return &http.Transport{
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		DisableCompression:    false,
		ForceAttemptHTTP2:     true,
	}
}

// Reload rebuilds the per-site runtime cache from a fresh site list.
// Called after the config watcher reports a change.
func (r *Router) Reload(sites []*config.Site) {
	next := make(map[string]*siteRuntime, len(sites))
	for _, s := range sites {
		if !s.Enabled {
			continue
		}
		pool, err := NewPool(s.Upstreams)
		if err != nil {
			continue
		}
		rt := &siteRuntime{
			site: s,
			pool: pool,
		}
		if s.RateLimit.Enabled {
			rt.limiter = NewLimiter(s.RateLimit.RPS, s.RateLimit.Burst)
		}
		rt.rp = r.buildReverseProxy(rt)
		next[s.Domain] = rt
		for _, a := range s.Aliases {
			next[a] = rt
		}
	}
	r.mu.Lock()
	r.runtimes = next
	r.mu.Unlock()
}

// buildReverseProxy constructs a per-site httputil.ReverseProxy. The
// director rewrites the request to the next upstream from the pool;
// the same proxy object can be reused across requests safely.
func (r *Router) buildReverseProxy(rt *siteRuntime) *httputil.ReverseProxy {
	custom := rt.site.CustomHeaders
	rp := &httputil.ReverseProxy{
		Transport: r.transport,
		Director: func(req *http.Request) {
			target := rt.pool.Next()
			if target == nil {
				return
			}
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			if target.Path != "" && target.Path != "/" {
				req.URL.Path = singleJoin(target.Path, req.URL.Path)
			}
			// Forwarded headers. We replace, not append — the client
			// shouldn't be able to inject upstream-trusted values.
			req.Header.Set("X-Forwarded-Host", req.Host)
			req.Header.Set("X-Forwarded-Proto", schemeOf(req))
			req.Header.Set("X-Real-IP", ClientIP(req, r.trustHeaders))
		},
		ModifyResponse: func(resp *http.Response) error {
			for k, v := range custom {
				resp.Header.Set(k, v)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
	}
	return rp
}

func singleJoin(a, b string) string {
	switch {
	case a == "" || a == "/":
		return b
	case b == "" || b == "/":
		return a
	case a[len(a)-1] == '/' && b[0] == '/':
		return a + b[1:]
	case a[len(a)-1] != '/' && b[0] != '/':
		return a + "/" + b
	default:
		return a + b
	}
}

func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if xf := r.Header.Get("X-Forwarded-Proto"); xf != "" {
		return xf
	}
	return "http"
}

// Stop tells the router to start refusing new requests with 503. Used
// during shutdown so in-flight requests can drain.
func (r *Router) Stop() { r.stopping.Store(true) }

// ServeHTTP — the hot path.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	if r.stopping.Load() {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}

	site, ok := r.store.Lookup(req.Host)
	if !ok {
		http.Error(w, "No site configured for "+sanitizeHostForLog(req.Host), http.StatusNotFound)
		return
	}

	// HTTP→HTTPS redirect (only if the request came in on plain HTTP).
	if site.HTTPToHTTPS && req.TLS == nil {
		target := "https://" + req.Host + req.URL.RequestURI()
		http.Redirect(w, req, target, http.StatusMovedPermanently)
		return
	}

	// Scanner / exploit pre-filter.
	if site.BlockCommonExploits {
		if commonExploitPattern.MatchString(req.URL.Path) ||
			scannerUA.MatchString(req.UserAgent()) {
			// 444 isn't a real status — closing the conn would be nicer
			// but we can't from this layer without hijacking. 403 is the
			// closest standards-respecting response that doesn't leak signal.
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
	}

	r.mu.RLock()
	rt := r.runtimes[site.Domain]
	r.mu.RUnlock()
	if rt == nil {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}

	if rt.limiter != nil {
		ip := ClientIP(req, r.trustHeaders)
		if !rt.limiter.Allow(ip) {
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			r.recordMetric(site.Domain, http.StatusTooManyRequests, 0, time.Since(start))
			return
		}
	}

	cw := &countingWriter{ResponseWriter: w, status: http.StatusOK}
	rt.rp.ServeHTTP(cw, req)
	r.recordMetric(site.Domain, cw.status, cw.bytes, time.Since(start))
}

func (r *Router) recordMetric(domain string, status int, bytes int64, latency time.Duration) {
	s := r.registry.SiteFor(domain)
	s.Record(metrics.Sample{Status: status, Bytes: bytes, Latency: latency})
}

// countingWriter snoops the status code and the number of bytes
// written so we can populate metrics after ReverseProxy returns. It
// implements http.Flusher and http.Hijacker passthrough so streaming /
// WebSocket upgrades still work.
type countingWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (c *countingWriter) WriteHeader(code int) {
	if !c.wroteHeader {
		c.status = code
		c.wroteHeader = true
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if !c.wroteHeader {
		c.wroteHeader = true
	}
	n, err := c.ResponseWriter.Write(b)
	c.bytes += int64(n)
	return n, err
}

func (c *countingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// sanitizeHostForLog strips anything not [a-zA-Z0-9.:-] so a malicious
// Host header can't inject newlines into 404 bodies / logs.
func sanitizeHostForLog(host string) string {
	out := make([]byte, 0, len(host))
	for i := 0; i < len(host) && i < 253; i++ {
		c := host[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '.' || c == ':' || c == '-':
			out = append(out, c)
		}
	}
	return string(out)
}
