package config

import (
	"context"
	"log/slog"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher watches sitesDir for changes and refreshes Store. Reloads are
// debounced (200ms) so a multi-file save doesn't trigger N rebuilds.
//
// OnReload is invoked AFTER the Store has been updated. Callers use it
// to (re)provision TLS certs for any newly added domain.
type Watcher struct {
	SitesDir string
	Store    *Store
	OnReload func(sites []*Site, errs []error)
}

// Run blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fw.Close()
	if err := fw.Add(w.SitesDir); err != nil {
		return err
	}

	// Initial load.
	w.reload()

	var (
		debounce  = 200 * time.Millisecond
		timerCh   <-chan time.Time
		armedTime time.Time
	)
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-fw.Events:
			if !ok {
				return nil
			}
			// Only react to writes/creates/renames/removes of site files.
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			armedTime = time.Now().Add(debounce)
			timerCh = time.After(debounce)
		case err, ok := <-fw.Errors:
			if !ok {
				return nil
			}
			slog.Warn("fsnotify error", "err", err)
		case <-timerCh:
			// Only fire if no further events came in during the debounce.
			if time.Now().Before(armedTime) {
				timerCh = time.After(time.Until(armedTime))
				continue
			}
			timerCh = nil
			w.reload()
		}
	}
}

func (w *Watcher) reload() {
	sites, errs := LoadSites(w.SitesDir)
	w.Store.Replace(sites)
	for _, e := range errs {
		slog.Warn("site load error", "err", e)
	}
	slog.Info("site config reloaded", "count", len(sites), "errors", len(errs))
	if w.OnReload != nil {
		w.OnReload(sites, errs)
	}
}
