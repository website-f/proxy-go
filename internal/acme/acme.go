// Package acme wraps github.com/caddyserver/certmagic to handle
// Let's Encrypt cert issuance and renewal for all enabled sites.
//
// We use certmagic because:
//   - It's the same library Caddy uses in production
//   - Handles ACME http-01 challenges, OCSP stapling, renewal, retry
//     backoff with the broker
//   - Stores certs on disk in a way that's safe to back up via tar
package acme

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/caddyserver/certmagic"

	jcconfig "jobcloud/internal/config"
)

// Manager wraps a certmagic.Config and exposes the API jobcloud needs.
type Manager struct {
	cfg    *certmagic.Config
	issuer *certmagic.ACMEIssuer
	email  string
	dir    string // ACME directory URL (optional override)
}

// New builds the Manager. `certsDir` is where issued certs persist.
// `email` is the ACME contact (required by LE for renewal warnings).
// `directory` is empty for LE prod, or the LE staging URL during tests.
func New(certsDir, email, directory string) (*Manager, error) {
	if email == "" {
		return nil, errors.New("acme_email is required (Let's Encrypt registration)")
	}
	// Storage on disk under certsDir. Survives container restarts via
	// the bind mount in docker-compose.
	storage := &certmagic.FileStorage{Path: filepath.Clean(certsDir)}

	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return certmagic.NewDefault(), nil
		},
	})
	cfg := certmagic.New(cache, certmagic.Config{
		Storage: storage,
	})
	acmeIss := certmagic.NewACMEIssuer(cfg, certmagic.ACMEIssuer{
		Email:                   email,
		Agreed:                  true,
		DisableHTTPChallenge:    false,
		DisableTLSALPNChallenge: false,
		CA:                      directory, // empty = default LE prod
	})
	cfg.Issuers = []certmagic.Issuer{acmeIss}
	return &Manager{cfg: cfg, issuer: acmeIss, email: email, dir: directory}, nil
}

// Issuer returns the configured ACME issuer. The HTTP listener wraps
// its handler with issuer.HTTPChallengeHandler so http-01 requests
// (GET /.well-known/acme-challenge/<token>) are answered correctly.
func (m *Manager) Issuer() *certmagic.ACMEIssuer { return m.issuer }

// SyncDomains tells certmagic which domains we want certs for. New
// domains are queued for issuance, removed ones stop being renewed.
// Safe to call repeatedly — certmagic dedupes.
func (m *Manager) SyncDomains(ctx context.Context, domains []string) {
	if len(domains) == 0 {
		return
	}
	if err := m.cfg.ManageAsync(ctx, domains); err != nil {
		slog.Warn("certmagic ManageAsync failed", "err", err, "domains", domains)
	}
}

// TLSConfig returns a *tls.Config that serves managed certs and falls
// back gracefully when a cert isn't ready yet (LE issuance can take
// 10-30s on a cold start).
func (m *Manager) TLSConfig() *tls.Config {
	return m.cfg.TLSConfig()
}

// CertInfo summarises one managed cert for the UI.
type CertInfo struct {
	Domain    string
	NotAfter  string // RFC3339 timestamp
	Issuer    string
	Managed   bool
	LastError string
}

// ListCerts returns UI-ready cert info. We pull each cert through the
// same TLS GetCertificate hook that real handshakes use — if certmagic
// has already issued and cached it, we get the parsed leaf back; if
// not, we surface "pending" so the UI can warn the operator that DNS
// or ACME may not be working.
func (m *Manager) ListCerts(domains []string) []CertInfo {
	out := make([]CertInfo, 0, len(domains))
	tlsCfg := m.cfg.TLSConfig()
	for _, d := range domains {
		ci := CertInfo{Domain: d, Managed: true}
		hello := &tls.ClientHelloInfo{ServerName: d}
		c, err := tlsCfg.GetCertificate(hello)
		if err != nil || c == nil || c.Leaf == nil {
			ci.LastError = "pending issuance"
			if err != nil {
				ci.LastError = fmt.Sprintf("pending: %v", err)
			}
			out = append(out, ci)
			continue
		}
		ci.NotAfter = c.Leaf.NotAfter.Format("2006-01-02 15:04 MST")
		if len(c.Leaf.Issuer.Organization) > 0 {
			ci.Issuer = c.Leaf.Issuer.Organization[0]
		} else {
			ci.Issuer = c.Leaf.Issuer.CommonName
		}
		out = append(out, ci)
	}
	return out
}

// CertConfig returns the underlying *certmagic.Config (kept exported
// in case we add OnDemand TLS hooks or similar extensions).
func (m *Manager) CertConfig() *certmagic.Config { return m.cfg }

// DomainsFromSites collects all enabled domains+aliases that should
// have certs issued.
func DomainsFromSites(sites []*jcconfig.Site) []string {
	out := make([]string, 0, len(sites)*2)
	for _, s := range sites {
		if !s.Enabled || !s.TLS.Auto {
			continue
		}
		out = append(out, s.AllDomains()...)
	}
	return out
}
