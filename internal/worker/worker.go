// Package worker runs the background loops that drive jobs to completion.
package worker

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
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

	// missingPolls counts, per TorBox ID, how many consecutive polls a job
	// has been absent from the TorBox list. Touched only by the single
	// poller goroutine, so it needs no lock.
	missingPolls map[int64]int

	// deleteAttempts counts, per job ID, failed TorBox-delete attempts.
	// Touched only by the single deleter goroutine, so it needs no lock.
	deleteAttempts map[int64]int

	// submitBackoffUntil pauses NZB submissions after TorBox rate-limits us
	// with a 429. Touched only by the single submitter goroutine, no lock.
	submitBackoffUntil time.Time

	// WebDAV /refresh state, also poller-goroutine-only.
	httpClient         *http.Client
	lastWebDAVRefresh  time.Time
	webdavBackoffUntil time.Time

	// healLastRun is the Unix-nanos timestamp of the last healOnce run,
	// read by the /health/symlinks endpoint. Atomic for cross-goroutine reads.
	healLastRun atomic.Int64
}

// New constructs a Workers.
func New(st *store.Store, tb TorBoxAPI, cfg *config.Config, logger *slog.Logger) *Workers {
	return &Workers{
		store:          st,
		tb:             tb,
		cfg:            cfg,
		logger:         logger,
		missingPolls:   map[int64]int{},
		deleteAttempts: map[int64]int{},
		httpClient:     &http.Client{Timeout: 30 * time.Second},
	}
}

// loopSpec is one background loop: a name, a tick interval, and the function
// run each tick.
type loopSpec struct {
	name     string
	interval time.Duration
	fn       func(context.Context) error
}

// Run starts every background loop and blocks until ctx is cancelled.
func (w *Workers) Run(ctx context.Context) {
	loops := []loopSpec{
		{"submitter", w.cfg.PollInterval, w.submitOnce},
		{"poller", w.cfg.PollInterval, w.pollOnce},
		{"deleter", w.cfg.PollInterval, w.deleteOnce},
		{"reaper", 5 * time.Minute, w.reapOnce},
	}
	if w.cfg.HealEnabled {
		loops = append(loops,
			loopSpec{"healer", w.cfg.HealInterval, w.healOnce},
			loopSpec{"heal-reconciler", w.cfg.PollInterval, w.healReconcileOnce},
		)
	}
	var wg sync.WaitGroup
	for _, l := range loops {
		wg.Add(1)
		go func(spec loopSpec) {
			defer wg.Done()
			w.loop(ctx, spec.name, spec.interval, spec.fn)
		}(l)
	}
	wg.Wait()
}

// HealRunInfo returns the last and next scheduled healer run. Both are zero
// before the first run or when healing is disabled.
func (w *Workers) HealRunInfo() (last, next time.Time) {
	n := w.healLastRun.Load()
	if n == 0 {
		return time.Time{}, time.Time{}
	}
	last = time.Unix(0, n)
	return last, last.Add(w.cfg.HealInterval)
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
