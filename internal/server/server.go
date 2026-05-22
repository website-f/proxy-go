// Package server wires the listeners (HTTP, HTTPS, admin UI) and
// owns their lifecycle. main.go calls server.Run and waits.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"jobcloud/internal/acme"
	"jobcloud/internal/auth"
	"jobcloud/internal/config"
	"jobcloud/internal/metrics"
	"jobcloud/internal/proxy"
	"jobcloud/internal/ui"
)

// Deps wires the long-lived components built in main.
type Deps struct {
	Global    *config.Global
	Store     *config.Store
	Watcher   *config.Watcher
	Router    *proxy.Router
	UI        *ui.Server
	Auth      *auth.Manager
	ACME      *acme.Manager
	Registry  *metrics.Registry
	StartedAt time.Time
}

// Run brings up the HTTP, HTTPS, and admin listeners. Blocks until
// ctx is cancelled, then drains gracefully.
func Run(ctx context.Context, d *Deps) error {
	// HTTP listener: serves ACME http-01 challenges + plain-HTTP
	// traffic + the optional redirect to HTTPS. We wrap the router
	// in certmagic's HTTPChallengeHandler so /.well-known/acme-challenge/
	// requests get answered correctly.
	httpHandler := d.ACME.Issuer().HTTPChallengeHandler(d.Router)

	httpSrv := &http.Server{
		Addr:              d.Global.HTTPAddr,
		Handler:           httpHandler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	// certmagic's TLSConfig sets GetCertificate, plus NextProtos for
	// the ALPN challenge. We add h2/http/1.1 so HTTP/2 negotiates.
	tlsCfg := d.ACME.TLSConfig()
	tlsCfg.NextProtos = append(tlsCfg.NextProtos, "h2", "http/1.1")
	httpsSrv := &http.Server{
		Addr:              d.Global.HTTPSAddr,
		Handler:           d.Router,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	adminSrv := &http.Server{
		Addr:              d.Global.AdminAddr,
		Handler:           d.UI.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	var (
		wg     sync.WaitGroup
		errCh  = make(chan error, 3)
		serve  = func(name string, run func() error) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := run(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					errCh <- err
					slog.Error("listener exited", "name", name, "err", err)
				}
			}()
		}
	)

	serve("http", func() error {
		slog.Info("HTTP listener up", "addr", d.Global.HTTPAddr)
		return httpSrv.ListenAndServe()
	})
	serve("https", func() error {
		slog.Info("HTTPS listener up", "addr", d.Global.HTTPSAddr)
		// ListenAndServeTLS with empty cert/key paths makes Go use
		// the GetCertificate hook on TLSConfig — which certmagic
		// fills with managed certs.
		return httpsSrv.ListenAndServeTLS("", "")
	})
	serve("admin", func() error {
		slog.Info("admin UI up", "addr", d.Global.AdminAddr)
		return adminSrv.ListenAndServe()
	})

	// Wait for shutdown signal or fatal listener error.
	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining...")
	case err := <-errCh:
		slog.Error("listener fatal", "err", err)
	}

	// Tell the router to start NACKing new traffic with 503 so
	// existing requests can finish cleanly while load balancers
	// notice the deregistration.
	d.Router.Stop()

	drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(drainCtx)
	_ = httpsSrv.Shutdown(drainCtx)
	_ = adminSrv.Shutdown(drainCtx)
	wg.Wait()
	return nil
}
