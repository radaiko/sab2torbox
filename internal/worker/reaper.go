package worker

import (
	"context"
	"time"
)

// importedTTL is how long an imported job is kept before the reaper drops it.
const importedTTL = 24 * time.Hour

// reapOnce deletes imported jobs older than importedTTL. It is a safety net
// for downloads Sonarr never explicitly deleted via the SAB API.
func (w *Workers) reapOnce(ctx context.Context) error {
	n, err := w.store.ReapImported(ctx, time.Now().Add(-importedTTL))
	if err != nil {
		return err
	}
	if n > 0 {
		w.logger.Info("reaped imported jobs", "count", n)
	}
	return nil
}
