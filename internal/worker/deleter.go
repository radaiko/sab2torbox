package worker

import (
	"context"

	"github.com/radaiko/sab2torbox/internal/job"
)

// deleteGiveUpAttempts bounds how many times the deleter retries a failing
// TorBox deletion before dropping the job row anyway. At the default 1m poll
// interval this is roughly an hour of retries. The download may then be left
// orphaned on TorBox, which is logged at error level.
var deleteGiveUpAttempts = 60

// deleteOnce removes from TorBox every job Sonarr asked to delete with its
// files. TorBox's controlusenetdownload is intermittently flaky (HTTP 500,
// "try again later"), so a failed attempt is retried on the next cycle rather
// than dropping the request.
func (w *Workers) deleteOnce(ctx context.Context) error {
	jobs, err := w.store.JobsByState(ctx, job.StateDeleted)
	if err != nil {
		return err
	}
	active := make(map[int64]bool, len(jobs))
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		active[j.ID] = true
		w.deleteJob(ctx, j)
	}
	// Drop retry counters for jobs no longer awaiting deletion.
	for id := range w.deleteAttempts {
		if !active[id] {
			delete(w.deleteAttempts, id)
		}
	}
	return nil
}

// deleteJob deletes one job's download from TorBox and removes its row. On a
// transient TorBox failure it keeps the row so the next cycle retries; once
// deleteGiveUpAttempts consecutive failures have elapsed it drops the row
// regardless.
func (w *Workers) deleteJob(ctx context.Context, j *job.Job) {
	log := w.logger.With("job_id", j.ID, "torbox_id", j.TorBoxID)

	if j.TorBoxID != 0 {
		if err := w.tb.ControlUsenet(ctx, j.TorBoxID, "delete"); err != nil {
			w.deleteAttempts[j.ID]++
			if w.deleteAttempts[j.ID] < deleteGiveUpAttempts {
				log.Warn("torbox delete failed; will retry next cycle",
					"attempt", w.deleteAttempts[j.ID], "error", err)
				return // keep the row
			}
			log.Error("torbox delete still failing; dropping job, download may be orphaned",
				"attempts", w.deleteAttempts[j.ID], "error", err)
		} else {
			log.Info("download deleted from torbox")
		}
	}
	delete(w.deleteAttempts, j.ID)
	// Remove the per-release symlink directory.
	if j.StoragePath != "" {
		if err := removeSymlinkDir(w.cfg.SymlinkRoot, j.StoragePath); err != nil {
			log.Warn("removing symlink directory", "dir", j.StoragePath, "error", err)
		}
	}
	if err := w.store.DeleteJob(ctx, j.ID); err != nil {
		log.Error("removing deleted job row", "error", err)
	}
}
