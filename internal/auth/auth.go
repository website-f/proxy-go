// Package auth implements admin login for the jobcloud web UI.
//
// Design:
//   - Admin users live in the global config file (bcrypt password
//     hashes). No separate user DB to maintain.
//   - Sessions are signed cookies (HMAC-SHA256). Stateless on the
//     server side — restart the binary and existing sessions keep
//     working until their expiry. Avoids needing a session store.
//   - 24h session lifetime. Cookie is HttpOnly, SameSite=Lax,
//     Secure when served over HTTPS.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"jobcloud/internal/config"
)

const (
	cookieName    = "jobcloud_session"
	sessionMaxAge = 24 * time.Hour
)

// Manager holds the active admin list + session signing secret.
type Manager struct {
	admins map[string]string // username -> bcrypt hash
	secret []byte
}

// New builds the Manager. If secret is empty, a persistent one is
// read/written at <dataDir>/secret.key so sessions survive restarts.
func New(g *config.Global, dataDir string) (*Manager, error) {
	if len(g.Admins) == 0 {
		return nil, errors.New("no admins configured — add one to config.yml")
	}
	admins := make(map[string]string, len(g.Admins))
	for _, a := range g.Admins {
		if a.Username == "" || a.PasswordHash == "" {
			return nil, fmt.Errorf("admin %q has empty username or password_hash", a.Username)
		}
		admins[strings.ToLower(a.Username)] = a.PasswordHash
	}

	secret, err := resolveSecret(g.SessionSecret, dataDir)
	if err != nil {
		return nil, err
	}
	return &Manager{admins: admins, secret: secret}, nil
}

func resolveSecret(configured, dataDir string) ([]byte, error) {
	if configured != "" {
		if len(configured) < 32 {
			return nil, errors.New("session_secret must be ≥ 32 characters")
		}
		return []byte(configured), nil
	}
	path := filepath.Join(dataDir, "secret.key")
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	// Generate + persist.
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return b, nil
}

// VerifyPassword returns nil if (username, plaintext) match an admin.
func (m *Manager) VerifyPassword(username, password string) error {
	hash, ok := m.admins[strings.ToLower(username)]
	if !ok {
		// Constant-time-ish: still run bcrypt against a throwaway hash
		// so a username probe can't time-distinguish valid users.
		_ = bcrypt.CompareHashAndPassword(
			[]byte("$2a$10$invalidinvalidinvalidinvalidinvalidinvalidinvalidinval"),
			[]byte(password),
		)
		return errors.New("invalid credentials")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return errors.New("invalid credentials")
	}
	return nil
}

// IssueCookie writes a signed session cookie to w. Caller has already
// verified the password.
func (m *Manager) IssueCookie(w http.ResponseWriter, r *http.Request, username string) {
	exp := time.Now().Add(sessionMaxAge).Unix()
	payload := fmt.Sprintf("%s.%d", strings.ToLower(username), exp)
	sig := m.sign(payload)
	value := base64.RawURLEncoding.EncodeToString([]byte(payload + "." + sig))
	c := &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		Expires:  time.Now().Add(sessionMaxAge),
	}
	http.SetCookie(w, c)
}

// ClearCookie deletes the session cookie.
func ClearCookie(w http.ResponseWriter, r *http.Request) {
	c := &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	}
	http.SetCookie(w, c)
}

// Authenticated returns the username if the request has a valid
// session cookie, or "" if not.
func (m *Manager) Authenticated(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return ""
	}
	parts := strings.SplitN(string(raw), ".", 3)
	if len(parts) != 3 {
		return ""
	}
	username, expStr, sig := parts[0], parts[1], parts[2]
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return ""
	}
	expected := m.sign(username + "." + expStr)
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return ""
	}
	if _, ok := m.admins[username]; !ok {
		// Admin was removed from config — invalidate session.
		return ""
	}
	return username
}

func (m *Manager) sign(payload string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// HashPassword is exposed so a future `jobcloud hash` CLI subcommand
// can generate password_hash values for config.yml.
func HashPassword(plaintext string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Require is middleware that redirects unauthenticated requests to
// /login.
func (m *Manager) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.Authenticated(r) == "" {
			http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}
