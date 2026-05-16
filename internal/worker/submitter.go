package worker

import (
	"context"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/radaiko/sab2torbox/internal/job"
	"github.com/radaiko/sab2torbox/internal/torbox"
)

// submitOnce submits every pending job to TorBox.
func (w *Workers) submitOnce(ctx context.Context) error {
	jobs, err := w.store.JobsByState(ctx, job.StatePending)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.submitJob(ctx, j)
	}
	return nil
}

// submitJob submits one job, retrying transient TorBox errors with backoff.
func (w *Workers) submitJob(ctx context.Context, j *job.Job) {
	log := w.logger.With("job_id", j.ID, "nzb_name", j.NZBName)
	j.State = job.StateSubmitting
	if err := w.store.UpdateJob(ctx, j); err != nil {
		log.Error("marking job submitting", "error", err)
		return
	}

	req := torbox.CreateRequest{
		NZBContent: j.NZBContent,
		NZBName:    j.NZBName + ".nzb",
		Link:       j.NZBURL,
	}

	var result *torbox.CreateResult
	bo := backoff.WithContext(newRetryPolicy(), ctx)
	err := backoff.Retry(func() error {
		r, err := w.tb.CreateUsenetDownload(ctx, req)
		if err != nil {
			if torbox.Retryable(err) {
				log.Warn("transient submit error, retrying", "error", err)
				return err
			}
			return backoff.Permanent(err)
		}
		result = r
		return nil
	}, bo)

	if err != nil {
		j.State = job.StateFailed
		j.FailMessage = "TorBox submission failed: " + err.Error()
		if uerr := w.store.UpdateJob(ctx, j); uerr != nil {
			log.Error("marking job failed", "error", uerr)
		}
		log.Error("submission permanently failed", "error", err)
		return
	}

	now := time.Now()
	j.State = job.StateQueued
	j.TorBoxID = int64(result.UsenetDownloadID)
	j.TorBoxHash = result.Hash
	j.SubmittedAt = &now
	if err := w.store.UpdateJob(ctx, j); err != nil {
		log.Error("marking job queued", "error", err)
		return
	}
	log.Info("job submitted to torbox", "torbox_id", j.TorBoxID)
}

// newRetryPolicy returns the backoff policy for transient TorBox errors:
// capped exponential backoff, max ~2 minutes total.
func newRetryPolicy() backoff.BackOff {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 2 * time.Second
	bo.MaxInterval = 30 * time.Second
	bo.MaxElapsedTime = 2 * time.Minute
	return bo
}
