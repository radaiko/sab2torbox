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

// missingPollThreshold is how many consecutive polls a job may be absent from
// the TorBox list before the poller declares it failed. At the default 1m
// poll interval this is six minutes of continuous absence, which debounces a
// transient mylist hiccup while still catching a download TorBox has dropped.
var missingPollThreshold = 6

// reconcileResult classifies a job after one poll so pollOnce can decide
// whether to force a TorBox WebDAV refresh.
type reconcileResult int

const (
	reconcileSettled        reconcileResult = iota // completed or failed; nothing pending
	reconcileOngoing                               // still transferring on TorBox
	reconcileAwaitingWebDAV                        // finished on TorBox, file not on the mount yet
)

// pollOnce fetches the TorBox list once and reconciles every active job.
func (w *Workers) pollOnce(ctx context.Context) error {
	jobs, err := w.store.JobsByState(ctx, activeStates...)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		clear(w.missingPolls)
		return nil
	}
	list, err := w.tb.ListUsenet(ctx)
	if err != nil {
		// A failed list call is not evidence a job is gone; don't count it.
		return fmt.Errorf("listing torbox usenet: %w", err)
	}
	byID := make(map[int64]torbox.UsenetDownload, len(list))
	for _, d := range list {
		byID[int64(d.ID)] = d
	}
	stillActive := make(map[int64]bool, len(jobs))
	var ongoing, awaiting bool
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		stillActive[j.TorBoxID] = true
		rec, ok := byID[j.TorBoxID]
		if !ok {
			if failed := w.handleMissing(ctx, j); !failed {
				ongoing = true // unknown state; treat as not finished
			}
			continue
		}
		delete(w.missingPolls, j.TorBoxID)
		switch w.reconcile(ctx, j, rec) {
		case reconcileOngoing:
			ongoing = true
		case reconcileAwaitingWebDAV:
			awaiting = true
		}
	}
	// Drop miss counters for jobs that are no longer active.
	for tbID := range w.missingPolls {
		if !stillActive[tbID] {
			delete(w.missingPolls, tbID)
		}
	}
	// Force a WebDAV refresh only when downloads have finished but their files
	// have not surfaced yet, and nothing is still transferring -- otherwise
	// TorBox's own 15-minute refresh will catch them.
	if awaiting && !ongoing {
		w.maybeRefreshWebDAV(ctx)
	}
	return nil
}

// handleMissing tracks an active job that did not appear in the TorBox list.
// After missingPollThreshold consecutive absences it fails the job so a
// download TorBox has silently dropped does not stay stuck forever. It reports
// whether the job was failed.
func (w *Workers) handleMissing(ctx context.Context, j *job.Job) bool {
	w.missingPolls[j.TorBoxID]++
	n := w.missingPolls[j.TorBoxID]
	log := w.logger.With("job_id", j.ID, "torbox_id", j.TorBoxID, "consecutive_misses", n)
	if n < missingPollThreshold {
		// Routine until the threshold escalates it to a failure below.
		log.Debug("active job missing from torbox list")
		return false
	}
	j.State = job.StateFailed
	j.FailMessage = fmt.Sprintf(
		"download no longer present on TorBox (absent from list for %d polls)", n)
	if err := w.store.UpdateJob(ctx, j); err != nil {
		log.Error("persisting failed state for missing job", "error", err)
		return false
	}
	delete(w.missingPolls, j.TorBoxID)
	log.Warn("job failed: vanished from torbox")
	return true
}

// reconcile applies one TorBox record to its job, persists any change, and
// classifies the outcome.
func (w *Workers) reconcile(ctx context.Context, j *job.Job, rec torbox.UsenetDownload) reconcileResult {
	log := w.logger.With("job_id", j.ID, "torbox_id", j.TorBoxID)

	if rec.Failed() {
		j.State = job.StateFailed
		j.FailMessage = "TorBox reported state: " + rec.DownloadState
		if err := w.store.UpdateJob(ctx, j); err != nil {
			log.Error("persisting failed state", "error", err)
		}
		log.Warn("job failed on torbox", "download_state", rec.DownloadState)
		return reconcileSettled
	}

	j.TotalBytes = rec.Size
	j.DownloadedBytes = rec.DownloadedBytes()
	j.ProgressPct = rec.ProgressPct()
	j.ETASeconds = rec.ETASeconds()

	if rec.DownloadFinished && rec.DownloadPresent {
		sourceDir, err := w.resolveStoragePath(ctx, rec.Name)
		if err != nil {
			// Routine while TorBox's WebDAV catches up — not actionable.
			log.Debug("waiting for webdav path", "error", err)
			// Keep progress; retry on the next poll.
			if uerr := w.store.UpdateJob(ctx, j); uerr != nil {
				log.Error("persisting progress", "error", uerr)
			}
			return reconcileAwaitingWebDAV
		}
		storagePath, files, ferr := buildSymlinkFarm(w.cfg.SymlinkRoot, j.Category, rec.Name, sourceDir)
		if ferr != nil {
			log.Error("building symlink farm", "error", ferr)
			if uerr := w.store.UpdateJob(ctx, j); uerr != nil {
				log.Error("persisting progress", "error", uerr)
			}
			return reconcileAwaitingWebDAV
		}
		if files == 0 {
			// TorBox's WebDAV surfaces the release folder before its
			// contents. Completing now would publish an empty directory that
			// the reaper later sweeps away, leaving Sonarr importing a path
			// that no longer exists. Drop the premature dir and wait.
			log.Debug("release folder present but empty; waiting for webdav contents",
				"source", sourceDir)
			if err := removeSymlinkDir(w.cfg.SymlinkRoot, storagePath); err != nil {
				log.Warn("removing premature empty symlink dir",
					"dir", storagePath, "error", err)
			}
			if uerr := w.store.UpdateJob(ctx, j); uerr != nil {
				log.Error("persisting progress", "error", uerr)
			}
			return reconcileAwaitingWebDAV
		}
		now := time.Now()
		j.State = job.StateCompleted
		j.StoragePath = storagePath
		j.ProgressPct = 100
		j.ETASeconds = 0
		j.CompletedAt = &now
		if err := w.store.UpdateJob(ctx, j); err != nil {
			log.Error("persisting completed state", "error", err)
			return reconcileSettled
		}
		log.Info("job completed", "storage_path", storagePath)
		return reconcileSettled
	}

	if j.State == job.StateQueued {
		j.State = job.StateDownloading
	}
	if err := w.store.UpdateJob(ctx, j); err != nil {
		log.Error("persisting progress", "error", err)
	}
	return reconcileOngoing
}

// pathRetryInterval and pathRetryTimeout govern WebDAV listing-lag tolerance.
var (
	pathRetryInterval = time.Second
	pathRetryTimeout  = 30 * time.Second
)

// resolveStoragePath returns the verified host path for a completed download.
// TorBox creates one folder per release directly under the configured Usenet
// path, named exactly as the download (rec.Name). It polls the filesystem
// because the WebDAV listing can lag TorBox's completion flag.
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
