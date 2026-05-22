// Package config defines the YAML schema and loader for jobcloud.
//
// The global config (config.yml) is loaded once at startup. Per-site
// configs (sites/*.yml) are loaded at startup and hot-reloaded on
// filesystem changes — see watcher.go.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Global is the top-level config read from <data>/config.yml.
type Global struct {
	// Admin UI bind address (default 127.0.0.1:8090). Bind to loopback
	// in production and reach it via SSH tunnel — never expose publicly.
	AdminAddr string `yaml:"admin_addr"`
	// HTTP listener (default :80). Used both for traffic and ACME http-01.
	HTTPAddr string `yaml:"http_addr"`
	// HTTPS listener (default :443).
	HTTPSAddr string `yaml:"https_addr"`
	// ACME contact email — required for Let's Encrypt issuance.
	ACMEEmail string `yaml:"acme_email"`
	// ACME directory URL. Empty = Let's Encrypt prod.
	ACMEDirectory string `yaml:"acme_directory"`
	// Admin user(s).
	Admins []Admin `yaml:"admins"`
	// Secret used to sign session cookies. 32+ random bytes.
	// If empty at startup, jobcloud generates one and writes it to
	// <data>/secret.key — persistent across restarts.
	SessionSecret string `yaml:"session_secret"`
	// Trust X-Forwarded-* headers (set true only if jobcloud sits
	// behind another L7 proxy — by default jobcloud IS the edge).
	TrustForwardedHeaders bool `yaml:"trust_forwarded_headers"`
}

// Admin is one admin login.
type Admin struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"` // bcrypt
}

// Site describes one routed domain → upstream(s).
type Site struct {
	// Filename the site was loaded from (set by loader, not in YAML).
	Filename string `yaml:"-"`

	// Public domain (server_name). Required.
	Domain string `yaml:"domain"`
	// Additional domain aliases (e.g. www.example.com).
	Aliases []string `yaml:"aliases"`
	// Upstreams to load-balance across. Each in the form host:port
	// (e.g. "127.0.0.1:8082"). Required ≥ 1.
	Upstreams []string `yaml:"upstreams"`
	// Enabled toggles serving without deleting the file.
	Enabled bool `yaml:"enabled"`
	// TLS settings.
	TLS TLSConfig `yaml:"tls"`
	// HTTPToHTTPS — if true, redirect 80 → 443.
	HTTPToHTTPS bool `yaml:"http_to_https"`
	// WebSocket — enable WS upgrade passthrough.
	WebSocket bool `yaml:"websocket"`
	// BlockCommonExploits — drop requests to /wp-admin, /.env, etc.
	BlockCommonExploits bool `yaml:"block_common_exploits"`
	// RateLimit — per-site rate limit (token bucket, per source IP).
	RateLimit RateLimitConfig `yaml:"rate_limit"`
	// CustomHeaders — extra response headers to add.
	CustomHeaders map[string]string `yaml:"custom_headers"`
}

// TLSConfig — per-site TLS settings.
type TLSConfig struct {
	// Auto — request a Let's Encrypt cert automatically.
	Auto bool `yaml:"auto"`
	// CertFile / KeyFile — manual cert paths (overrides Auto).
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// RateLimitConfig — leaky-bucket per source IP.
type RateLimitConfig struct {
	Enabled bool `yaml:"enabled"`
	// Sustained requests per second.
	RPS int `yaml:"rps"`
	// Burst capacity above sustained.
	Burst int `yaml:"burst"`
}

// AllDomains returns Domain + Aliases.
func (s *Site) AllDomains() []string {
	d := make([]string, 0, 1+len(s.Aliases))
	d = append(d, s.Domain)
	d = append(d, s.Aliases...)
	return d
}

// Validate checks site config and returns the first error found.
func (s *Site) Validate() error {
	if strings.TrimSpace(s.Domain) == "" {
		return errors.New("domain is required")
	}
	if len(s.Upstreams) == 0 {
		return errors.New("at least one upstream is required")
	}
	for _, u := range s.Upstreams {
		if !validUpstream(u) {
			return fmt.Errorf("invalid upstream %q (expected host:port)", u)
		}
	}
	if s.RateLimit.Enabled {
		if s.RateLimit.RPS <= 0 {
			return errors.New("rate_limit.rps must be > 0 when enabled")
		}
		if s.RateLimit.Burst < 0 {
			return errors.New("rate_limit.burst must be ≥ 0")
		}
	}
	return nil
}

func validUpstream(s string) bool {
	// Allow either host:port or http(s)://host:port/path.
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return false
		}
		return true
	}
	if !strings.Contains(s, ":") {
		return false
	}
	host, port, err := splitHostPort(s)
	if err != nil || host == "" || port == "" {
		return false
	}
	return true
}

func splitHostPort(s string) (string, string, error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", "", errors.New("no port")
	}
	return s[:i], s[i+1:], nil
}

// LoadGlobal reads config.yml from dataDir.
func LoadGlobal(dataDir string) (*Global, error) {
	path := filepath.Join(dataDir, "config.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var g Global
	if err := yaml.Unmarshal(b, &g); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	g.applyDefaults()
	return &g, nil
}

func (g *Global) applyDefaults() {
	if g.AdminAddr == "" {
		g.AdminAddr = "127.0.0.1:8090"
	}
	if g.HTTPAddr == "" {
		g.HTTPAddr = ":80"
	}
	if g.HTTPSAddr == "" {
		g.HTTPSAddr = ":443"
	}
}

// LoadSites scans sitesDir for *.yml/*.yaml files and parses them.
// Invalid files are returned in errs but do not abort the load — this
// matches NPM's "broken site doesn't take down the whole proxy" behavior.
func LoadSites(sitesDir string) (sites []*Site, errs []error) {
	entries, err := os.ReadDir(sitesDir)
	if err != nil {
		return nil, []error{fmt.Errorf("read sites dir: %w", err)}
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !(strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")) {
			continue
		}
		path := filepath.Join(sitesDir, name)
		s, err := loadSiteFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		sites = append(sites, s)
	}
	sort.Slice(sites, func(i, j int) bool {
		return sites[i].Domain < sites[j].Domain
	})
	return sites, errs
}

func loadSiteFile(path string) (*Site, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Site
	if err := yaml.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	s.Filename = filepath.Base(path)
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// SaveSite writes a site config back to disk. Used by the UI to persist
// edits. Returns the resolved path.
func SaveSite(sitesDir string, s *Site) (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	name := s.Filename
	if name == "" {
		name = sanitizeFilename(s.Domain) + ".yml"
	}
	path := filepath.Join(sitesDir, name)
	b, err := yaml.Marshal(s)
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// DeleteSite removes a site config file.
func DeleteSite(sitesDir, filename string) error {
	if filename == "" || strings.ContainsAny(filename, "/\\") {
		return errors.New("invalid filename")
	}
	return os.Remove(filepath.Join(sitesDir, filename))
}

func sanitizeFilename(domain string) string {
	out := strings.ToLower(domain)
	out = strings.ReplaceAll(out, "/", "_")
	out = strings.ReplaceAll(out, "\\", "_")
	out = strings.ReplaceAll(out, "..", "_")
	return out
}

// Store is the in-memory, thread-safe holder of the current site list.
// The proxy and UI read from it. The watcher writes to it.
type Store struct {
	mu    sync.RWMutex
	sites []*Site
	// byDomain maps a Host header (lowercased) to its site for O(1)
	// routing on every request.
	byDomain map[string]*Site
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{byDomain: map[string]*Site{}}
}

// Replace atomically swaps the site list.
func (s *Store) Replace(sites []*Site) {
	idx := make(map[string]*Site, len(sites)*2)
	for _, site := range sites {
		if !site.Enabled {
			continue
		}
		for _, d := range site.AllDomains() {
			idx[strings.ToLower(d)] = site
		}
	}
	s.mu.Lock()
	s.sites = sites
	s.byDomain = idx
	s.mu.Unlock()
}

// Lookup returns the site (if any) for the given Host header.
func (s *Store) Lookup(host string) (*Site, bool) {
	// Strip any :port suffix.
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(host)
	s.mu.RLock()
	defer s.mu.RUnlock()
	site, ok := s.byDomain[host]
	return site, ok
}

// Snapshot returns a copy of the current site list (for the UI).
func (s *Store) Snapshot() []*Site {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Site, len(s.sites))
	copy(out, s.sites)
	return out
}

// Domains returns all (lowercased) domains+aliases currently served —
// used to seed certmagic with the cert list at startup.
func (s *Store) Domains() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.byDomain))
	for d := range s.byDomain {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}
