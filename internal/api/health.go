package api

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/radaiko/sab2torbox/internal/store"
)

// Pinger validates TorBox reachability. The TorBox client satisfies it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Health checks database reachability and TorBox token validity. The TorBox
// result is cached to avoid hammering the API on frequent /healthz probes.
type Health struct {
	store *store.Store
	tb    Pinger
	ttl   time.Duration

	mu         sync.Mutex
	lastCheck  time.Time
	lastResult error
}

// NewHealth constructs a Health with the given TorBox-result cache TTL.
func NewHealth(st *store.Store, tb Pinger, ttl time.Duration) *Health {
	return &Health{store: st, tb: tb, ttl: ttl}
}

// Check returns nil when the service is healthy.
func (h *Health) Check(ctx context.Context) error {
	if err := h.store.Ping(ctx); err != nil {
		return fmt.Errorf("database unreachable: %w", err)
	}
	return h.checkTorBox(ctx)
}

// checkTorBox pings TorBox at most once per ttl.
func (h *Health) checkTorBox(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.lastCheck.IsZero() && time.Since(h.lastCheck) < h.ttl {
		return h.lastResult
	}
	err := h.tb.Ping(ctx)
	h.lastCheck = time.Now()
	if err != nil {
		h.lastResult = fmt.Errorf("torbox unreachable: %w", err)
	} else {
		h.lastResult = nil
	}
	return h.lastResult
}
