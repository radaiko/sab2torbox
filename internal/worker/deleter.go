package worker

import (
	"context"
	"time"

	"github.com/radaiko/sab2torbox/internal/job"
)

// deleteGiveUpAfter bounds how long the deleter retries a failing TorBox
// deletion before dropping the job row anyway. The download may then be left
// orphaned on TorBox, which is logged at error level.
var deleteGiveUpAfter = time.Hour

// deleteOnce removes from TorBox every job Sonarr asked to delete with its
// files. TorBox's controlusenetdownload is intermittently flaky (HTTP 500,
// "try again later"), so a failed attempt is retried on the next cycle rather
// than dropping the request.
func (w *Workers) deleteOnce(ctx context.Context) error {
	jobs, err := w.store.JobsByState(ctx, job.StateDeleted)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.deleteJob(ctx, j)
	}
	return nil
}

// deleteJob deletes one job's download from TorBox and removes its row. On a
// transient TorBox failure it keeps the row so the next cycle retries; once
// deleteGiveUpAfter has elapsed it drops the row regardless.
func (w *Workers) deleteJob(ctx context.Context, j *job.Job) {
	log := w.logger.With("job_id", j.ID, "torbox_id", j.TorBoxID)

	if j.TorBoxID != 0 {
		if err := w.tb.ControlUsenet(ctx, j.TorBoxID, "delete"); err != nil {
			if time.Since(j.UpdatedAt) < deleteGiveUpAfter {
				log.Warn("torbox delete failed; will retry next cycle", "error", err)
				return // keep the row
			}
			log.Error("torbox delete still failing; dropping job, download may be orphaned",
				"error", err)
		} else {
			log.Info("download deleted from torbox")
		}
	}
	// Remove the per-release symlink directory, if symlink mode created one.
	if w.cfg.SymlinkModeEnabled() && j.StoragePath != "" {
		if err := removeSymlinkDir(w.cfg.SymlinkRoot, j.StoragePath); err != nil {
			log.Warn("removing symlink directory", "dir", j.StoragePath, "error", err)
		}
	}
	if err := w.store.DeleteJob(ctx, j.ID); err != nil {
		log.Error("removing deleted job row", "error", err)
	}
}
