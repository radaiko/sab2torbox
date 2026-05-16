package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/radaiko/sab2torbox/internal/job"
	"github.com/radaiko/sab2torbox/internal/torbox"
)

// activeStates are the job states the poller tracks against TorBox.
var activeStates = []job.State{job.StateQueued, job.StateDownloading}

// pollOnce fetches the TorBox list once and reconciles every active job.
func (w *Workers) pollOnce(ctx context.Context) error {
	jobs, err := w.store.JobsByState(ctx, activeStates...)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return nil
	}
	list, err := w.tb.ListUsenet(ctx)
	if err != nil {
		return fmt.Errorf("listing torbox usenet: %w", err)
	}
	byID := make(map[int64]torbox.UsenetDownload, len(list))
	for _, d := range list {
		byID[int64(d.ID)] = d
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rec, ok := byID[j.TorBoxID]
		if !ok {
			w.logger.Warn("active job missing from torbox list",
				"job_id", j.ID, "torbox_id", j.TorBoxID)
			continue
		}
		w.reconcile(ctx, j, rec)
	}
	return nil
}

// reconcile applies one TorBox record to its job and persists any change.
func (w *Workers) reconcile(ctx context.Context, j *job.Job, rec torbox.UsenetDownload) {
	log := w.logger.With("job_id", j.ID, "torbox_id", j.TorBoxID)

	if rec.Failed() {
		j.State = job.StateFailed
		j.FailMessage = "TorBox reported state: " + rec.DownloadState
		if err := w.store.UpdateJob(ctx, j); err != nil {
			log.Error("persisting failed state", "error", err)
		}
		log.Warn("job failed on torbox", "download_state", rec.DownloadState)
		return
	}

	j.TotalBytes = rec.Size
	j.DownloadedBytes = rec.DownloadedBytes()
	j.ProgressPct = rec.ProgressPct()

	if rec.DownloadFinished && rec.DownloadPresent {
		path, err := w.resolveStoragePath(ctx, rec.Name)
		if err != nil {
			log.Warn("waiting for webdav path", "error", err)
			// Keep progress; retry on the next poll.
			if uerr := w.store.UpdateJob(ctx, j); uerr != nil {
				log.Error("persisting progress", "error", uerr)
			}
			return
		}
		now := time.Now()
		j.State = job.StateCompleted
		j.StoragePath = path
		j.ProgressPct = 100
		j.CompletedAt = &now
		if err := w.store.UpdateJob(ctx, j); err != nil {
			log.Error("persisting completed state", "error", err)
			return
		}
		log.Info("job completed", "storage_path", path)
		return
	}

	if j.State == job.StateQueued {
		j.State = job.StateDownloading
	}
	if err := w.store.UpdateJob(ctx, j); err != nil {
		log.Error("persisting progress", "error", err)
	}
}

// pathRetryInterval and pathRetryTimeout govern WebDAV listing-lag tolerance.
var (
	pathRetryInterval = time.Second
	pathRetryTimeout  = 30 * time.Second
)

// resolveStoragePath returns the verified host path for a completed download.
// TorBox flags completion a few seconds before the WebDAV listing updates, so
// it polls the filesystem for the expected directory.
func (w *Workers) resolveStoragePath(ctx context.Context, name string) (string, error) {
	expected := filepath.Join(w.cfg.UsenetPath(), name)
	deadline := time.Now().Add(pathRetryTimeout)
	for {
		if info, err := os.Stat(expected); err == nil && info.IsDir() {
			return expected, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("path %q not present after %s", expected, pathRetryTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pathRetryInterval):
		}
	}
}
