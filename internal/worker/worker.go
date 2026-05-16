// Package worker runs the background loops that drive jobs to completion.
package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/radaiko/sab2torbox/internal/config"
	"github.com/radaiko/sab2torbox/internal/store"
	"github.com/radaiko/sab2torbox/internal/torbox"
)

// TorBoxAPI is the subset of the TorBox client the workers depend on.
type TorBoxAPI interface {
	CreateUsenetDownload(ctx context.Context, req torbox.CreateRequest) (*torbox.CreateResult, error)
	ListUsenet(ctx context.Context) ([]torbox.UsenetDownload, error)
	ControlUsenet(ctx context.Context, id int64, op string) error
	Ping(ctx context.Context) error
}

// Workers owns the Submitter, Poller, and Reaper background loops.
type Workers struct {
	store  *store.Store
	tb     TorBoxAPI
	cfg    *config.Config
	logger *slog.Logger
}

// New constructs a Workers.
func New(st *store.Store, tb TorBoxAPI, cfg *config.Config, logger *slog.Logger) *Workers {
	return &Workers{store: st, tb: tb, cfg: cfg, logger: logger}
}

// Run starts all three loops and blocks until ctx is cancelled.
func (w *Workers) Run(ctx context.Context) {
	var wg sync.WaitGroup
	loops := []struct {
		name     string
		interval time.Duration
		fn       func(context.Context) error
	}{
		{"submitter", w.cfg.PollInterval, w.submitOnce},
		{"poller", w.cfg.PollInterval, w.pollOnce},
		{"reaper", 5 * time.Minute, w.reapOnce},
	}
	for _, l := range loops {
		wg.Add(1)
		go func(name string, interval time.Duration, fn func(context.Context) error) {
			defer wg.Done()
			w.loop(ctx, name, interval, fn)
		}(l.name, l.interval, l.fn)
	}
	wg.Wait()
}

// loop runs fn immediately, then every interval, until ctx is cancelled.
func (w *Workers) loop(ctx context.Context, name string, interval time.Duration, fn func(context.Context) error) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			w.logger.Error("worker iteration failed", "worker", name, "error", err)
		}
		select {
		case <-ctx.Done():
			w.logger.Info("worker stopped", "worker", name)
			return
		case <-t.C:
		}
	}
}
