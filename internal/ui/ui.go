// Package ui is the admin web interface for jobcloud.
package ui

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"jobcloud/internal/acme"
	"jobcloud/internal/auth"
	"jobcloud/internal/config"
	"jobcloud/internal/metrics"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Server bundles all UI dependencies and the template set. Wire it
// once at startup and call Handler() to obtain an http.Handler.
type Server struct {
	Store    *config.Store
	Metrics  *metrics.Registry
	Auth     *auth.Manager
	ACME     *acme.Manager
	SitesDir string
	Version  string
	StartAt  time.Time
	// ReloadCallback is invoked after the UI mutates a site file —
	// used to re-trigger the watcher's reload synchronously so the
	// UI shows the change immediately. Optional.
	ReloadCallback func(ctx context.Context)

	tpl    *template.Template
	static http.Handler
}

// New parses templates and returns a ready Server.
func New(s *Server) (*Server, error) {
	funcs := template.FuncMap{
		"join": func(items []string, sep string) string { return strings.Join(items, sep) },
	}
	tpl, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tpl = tpl

	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	s.static = http.StripPrefix("/static/", http.FileServer(http.FS(staticFS)))

	return s, nil
}

// Handler returns the routing mux for the admin UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/static/", s.static.ServeHTTP)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)

	// Authenticated routes.
	authed := http.NewServeMux()
	authed.HandleFunc("/", s.handleDashboard)
	authed.HandleFunc("/sites", s.handleSitesList)
	authed.HandleFunc("/sites/new", s.handleSiteNew)
	authed.HandleFunc("/sites/", s.handleSitesItem) // /sites/<file>, /sites/<file>/toggle, /sites/<file>/delete
	authed.HandleFunc("/certs", s.handleCerts)
	authed.HandleFunc("/partials/site-rows", s.handleSiteRowsPartial)

	mux.Handle("/", s.Auth.Require(authed))
	return mux
}

// ---- Helpers ----

type baseData struct {
	Title           string
	Active          string
	Version         string
	SiteCount       int
	Uptime          string
	ContentTemplate string // name of the body template the layout should render
}

func (s *Server) base(title, active, contentTpl string) baseData {
	return baseData{
		Title:           title,
		Active:          active,
		Version:         s.Version,
		SiteCount:       len(s.Store.Snapshot()),
		Uptime:          humanDuration(time.Since(s.StartAt)),
		ContentTemplate: contentTpl,
	}
}

func humanDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d < time.Minute {
		return d.String()
	}
	if d < time.Hour {
		return strconv.Itoa(int(d.Minutes())) + "m"
	}
	if d < 24*time.Hour {
		return strconv.Itoa(int(d.Hours())) + "h " + strconv.Itoa(int(d.Minutes())%60) + "m"
	}
	days := int(d.Hours()) / 24
	return strconv.Itoa(days) + "d " + strconv.Itoa(int(d.Hours())%24) + "h"
}

func (s *Server) render(w http.ResponseWriter, body string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := s.tpl.ExecuteTemplate(w, body, data); err != nil {
		slog.Error("template render", "tpl", body, "err", err)
	}
}

// ---- Auth ----

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	type loginData struct {
		Error string
		Next  string
	}
	next := r.URL.Query().Get("next")
	if next == "" || !strings.HasPrefix(next, "/") {
		next = "/"
	}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		username := r.FormValue("username")
		password := r.FormValue("password")
		nextField := r.FormValue("next")
		if nextField != "" && strings.HasPrefix(nextField, "/") {
			next = nextField
		}
		if err := s.Auth.VerifyPassword(username, password); err != nil {
			s.render(w, "login", loginData{Error: "Invalid username or password.", Next: next})
			return
		}
		s.Auth.IssueCookie(w, r, username)
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.render(w, "login", loginData{Next: next})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	auth.ClearCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- Dashboard ----

type siteRow struct {
	Domain      string
	Aliases     []string
	Filename    string
	Enabled     bool
	Upstreams   []string
	TLS         config.TLSConfig
	ReqsPerMin  uint64
	BytesPerMin uint64
	ErrsPerMin  uint64
	P95         uint32
}

type dashboardData struct {
	baseData
	ActiveSites int
	ReqsPerMin  uint64
	BytesPerMin string
	ErrorRate   float64
	Sites       []siteRow
}

func (s *Server) buildSiteRows() (rows []siteRow, totalReqs, totalErrs, totalBytes uint64, active int) {
	sites := s.Store.Snapshot()
	snaps := s.Metrics.SnapshotAll()
	for _, site := range sites {
		snap := snaps[site.Domain]
		rows = append(rows, siteRow{
			Domain:      site.Domain,
			Aliases:     site.Aliases,
			Filename:    site.Filename,
			Enabled:     site.Enabled,
			Upstreams:   site.Upstreams,
			TLS:         site.TLS,
			ReqsPerMin:  snap.ReqsLast1m,
			BytesPerMin: snap.BytesLast1m,
			ErrsPerMin:  snap.ErrsLast1m,
			P95:         snap.P95,
		})
		totalReqs += snap.ReqsLast1m
		totalErrs += snap.ErrsLast1m
		totalBytes += snap.BytesLast1m
		if site.Enabled {
			active++
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Domain < rows[j].Domain })
	return
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	rows, reqs, errs, bytes, active := s.buildSiteRows()
	rate := 0.0
	if reqs > 0 {
		rate = float64(errs) / float64(reqs) * 100
	}
	data := dashboardData{
		baseData:    s.base("Dashboard", "dashboard", "dashboard"),
		ActiveSites: active,
		ReqsPerMin:  reqs,
		BytesPerMin: humanBytes(bytes),
		ErrorRate:   rate,
		Sites:       rows,
	}
	s.render(w, "layout", data)
}

func (s *Server) handleSiteRowsPartial(w http.ResponseWriter, r *http.Request) {
	rows, _, _, _, _ := s.buildSiteRows()
	data := struct{ Sites []siteRow }{Sites: rows}
	s.render(w, "site-rows", data)
}

func humanBytes(n uint64) string {
	const k = 1024
	switch {
	case n < k:
		return strconv.FormatUint(n, 10) + " B"
	case n < k*k:
		return strconv.FormatFloat(float64(n)/k, 'f', 1, 64) + " KB"
	case n < k*k*k:
		return strconv.FormatFloat(float64(n)/(k*k), 'f', 1, 64) + " MB"
	default:
		return strconv.FormatFloat(float64(n)/(k*k*k), 'f', 2, 64) + " GB"
	}
}

// ---- Sites list / item ----

func (s *Server) handleSitesList(w http.ResponseWriter, r *http.Request) {
	// /sites POST → create new site
	if r.Method == http.MethodPost {
		s.createOrUpdateSite(w, r, nil)
		return
	}
	// /sites GET → list (redirect to dashboard, which already lists them)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleSiteNew(w http.ResponseWriter, r *http.Request) {
	type data struct {
		baseData
		New   bool
		Site  *config.Site
		Error string
		Saved bool
	}
	s.render(w, "layout", data{
		baseData: s.base("Add site", "sites", "site-form"),
		New:      true,
		Site: &config.Site{
			Enabled:             true,
			HTTPToHTTPS:         true,
			WebSocket:           true,
			BlockCommonExploits: true,
			TLS:                 config.TLSConfig{Auto: true},
		},
	})
}

func (s *Server) handleSitesItem(w http.ResponseWriter, r *http.Request) {
	// path is /sites/<filename>[/toggle|/delete]
	path := strings.TrimPrefix(r.URL.Path, "/sites/")
	if path == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	parts := strings.SplitN(path, "/", 2)
	filename := parts[0]

	if strings.ContainsAny(filename, "/\\") || strings.Contains(filename, "..") {
		http.Error(w, "bad filename", http.StatusBadRequest)
		return
	}

	// Find the site.
	var site *config.Site
	for _, candidate := range s.Store.Snapshot() {
		if candidate.Filename == filename {
			site = candidate
			break
		}
	}

	// Sub-action?
	if len(parts) == 2 {
		switch parts[1] {
		case "toggle":
			if site == nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			site.Enabled = !site.Enabled
			if _, err := config.SaveSite(s.SitesDir, site); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			s.triggerReload(r.Context())
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		case "delete":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if err := config.DeleteSite(s.SitesDir, filename); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			s.triggerReload(r.Context())
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}

	// GET / POST on /sites/<filename>
	if r.Method == http.MethodPost {
		if site == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.createOrUpdateSite(w, r, site)
		return
	}

	if site == nil {
		http.NotFound(w, r)
		return
	}
	type data struct {
		baseData
		New   bool
		Site  *config.Site
		Error string
		Saved bool
	}
	s.render(w, "layout", data{
		baseData: s.base(site.Domain, "sites", "site-form"),
		Site:     site,
	})
}

func (s *Server) createOrUpdateSite(w http.ResponseWriter, r *http.Request, existing *config.Site) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var site config.Site
	if existing != nil {
		site = *existing
	}
	site.Domain = strings.TrimSpace(r.FormValue("domain"))
	site.Aliases = splitWords(r.FormValue("aliases"))
	site.Upstreams = splitLines(r.FormValue("upstreams"))
	site.Enabled = r.FormValue("enabled") == "on"
	site.HTTPToHTTPS = r.FormValue("http_to_https") == "on"
	site.WebSocket = r.FormValue("websocket") == "on"
	site.BlockCommonExploits = r.FormValue("block_common_exploits") == "on"
	site.TLS.Auto = r.FormValue("tls_auto") == "on"
	site.RateLimit.Enabled = r.FormValue("rl_enabled") == "on"
	if v, err := strconv.Atoi(r.FormValue("rl_rps")); err == nil {
		site.RateLimit.RPS = v
	}
	if v, err := strconv.Atoi(r.FormValue("rl_burst")); err == nil {
		site.RateLimit.Burst = v
	}

	if _, err := config.SaveSite(s.SitesDir, &site); err != nil {
		type data struct {
			baseData
			New   bool
			Site  *config.Site
			Error string
			Saved bool
		}
		s.render(w, "layout", data{
			baseData: s.base("Add site", "sites", "site-form"),
			New:      existing == nil,
			Site:     &site,
			Error:    err.Error(),
		})
		return
	}
	s.triggerReload(r.Context())
	http.Redirect(w, r, "/sites/"+site.Filename+"?saved=1", http.StatusSeeOther)
}

func (s *Server) triggerReload(ctx context.Context) {
	if s.ReloadCallback != nil {
		s.ReloadCallback(ctx)
	}
}

func splitWords(s string) []string {
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func splitLines(s string) []string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// ---- Certs ----

func (s *Server) handleCerts(w http.ResponseWriter, r *http.Request) {
	domains := s.Store.Domains()
	var infos []acme.CertInfo
	if s.ACME != nil {
		infos = s.ACME.ListCerts(domains)
	}
	type data struct {
		baseData
		Certs []acme.CertInfo
	}
	s.render(w, "layout", data{
		baseData: s.base("Certificates", "certs", "certs"),
		Certs:    infos,
	})
}
