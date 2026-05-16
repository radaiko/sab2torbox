package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/radaiko/sab2torbox/internal/job"
)

// webhookJob is the job summary embedded in a heal webhook payload.
type webhookJob struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Category  string `json:"category"`
	HealCount int64  `json:"heal_count"`
}

// webhookPayload is the JSON body POSTed to HealWebhookURL on a heal event.
type webhookPayload struct {
	Event          string     `json:"event"`
	Timestamp      string     `json:"timestamp"`
	Job            webhookJob `json:"job"`
	SymlinksHealed int        `json:"symlinks_healed,omitempty"`
	OldTorBoxID    int64      `json:"old_torbox_id,omitempty"`
	NewTorBoxID    int64      `json:"new_torbox_id,omitempty"`
	Error          string     `json:"error,omitempty"`
}

// healEventExtra carries the event-specific optional fields a caller supplies.
type healEventExtra struct {
	SymlinksHealed int
	OldTorBoxID    int64
	NewTorBoxID    int64
	Error          string
}

// healWebhookWants reports whether event is in the configured event set.
func healWebhookWants(events []string, event string) bool {
	for _, e := range events {
		if e == event {
			return true
		}
	}
	return false
}

// emitHealEvent fires a heal webhook for event, if a webhook URL is configured
// and event is in HealWebhookEvents. The POST runs in its own goroutine so a
// slow or unreachable webhook endpoint never stalls the healer.
func (w *Workers) emitHealEvent(event string, j *job.Job, extra healEventExtra) {
	if w.cfg.HealWebhookURL == "" || !healWebhookWants(w.cfg.HealWebhookEvents, event) {
		return
	}
	payload := webhookPayload{
		Event:     event,
		Timestamp: timeNow().UTC().Format(time.RFC3339),
		Job: webhookJob{
			ID: j.ID, Name: j.NZBName, Category: j.Category, HealCount: j.HealCount,
		},
		SymlinksHealed: extra.SymlinksHealed,
		OldTorBoxID:    extra.OldTorBoxID,
		NewTorBoxID:    extra.NewTorBoxID,
		Error:          extra.Error,
	}
	go w.postHealWebhook(payload)
}

// postHealWebhook delivers one webhook payload, best-effort. Failures are
// logged, never retried — the webhook is a notification, not a guarantee.
func (w *Workers) postHealWebhook(payload webhookPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		w.logger.Error("heal webhook: encoding payload", "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.HealWebhookURL,
		bytes.NewReader(body))
	if err != nil {
		w.logger.Error("heal webhook: building request", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.httpClient.Do(req)
	if err != nil {
		w.logger.Warn("heal webhook: post failed", "event", payload.Event, "error", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		w.logger.Warn("heal webhook: non-2xx response",
			"event", payload.Event, "status", resp.StatusCode)
	}
}
