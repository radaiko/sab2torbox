package worker

import (
	"context"
	"net/http"
	"time"
)

// webdavRefreshBackoff is how long to stop calling /refresh after TorBox
// rate-limits it (HTTP 429). TorBox's own listing refreshes every 15 minutes
// regardless, so a 15-minute backoff costs nothing.
var webdavRefreshBackoff = 15 * time.Minute

// maybeRefreshWebDAV forces a TorBox WebDAV listing refresh by hitting the
// /refresh endpoint, so a completed download surfaces in seconds instead of
// waiting out TorBox's 15-minute refresh cycle.
//
// It is a no-op unless the WebDAV credentials are configured. pollOnce only
// calls it when every active download has finished, so it never fires while
// transfers are in progress. It is further debounced by the configured
// cooldown and backs off hard on an HTTP 429.
func (w *Workers) maybeRefreshWebDAV(ctx context.Context) {
	if !w.cfg.WebDAVRefreshEnabled() {
		return
	}
	now := time.Now()
	if now.Before(w.webdavBackoffUntil) {
		return
	}
	if !w.lastWebDAVRefresh.IsZero() && now.Sub(w.lastWebDAVRefresh) < w.cfg.WebDAVRefreshCooldown {
		return
	}
	w.lastWebDAVRefresh = now

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.cfg.TorBoxWebDAVRefreshURL, nil)
	if err != nil {
		w.logger.Error("building webdav refresh request", "error", err)
		return
	}
	req.SetBasicAuth(w.cfg.TorBoxWebDAVUser, w.cfg.TorBoxWebDAVPass)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		w.logger.Warn("webdav refresh request failed", "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		w.webdavBackoffUntil = now.Add(webdavRefreshBackoff)
		w.logger.Warn("webdav refresh rate-limited; backing off",
			"backoff", webdavRefreshBackoff.String())
	case resp.StatusCode >= 400:
		w.logger.Warn("webdav refresh returned error status", "status", resp.StatusCode)
	default:
		w.logger.Info("forced torbox webdav refresh", "status", resp.StatusCode)
	}
}
