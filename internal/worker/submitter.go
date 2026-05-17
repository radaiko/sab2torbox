package worker

import (
	"context"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/radaiko/sab2torbox/internal/job"
	"github.com/radaiko/sab2torbox/internal/torbox"
)

// defaultRateLimitCooldown is how long the submitter pauses after a TorBox
// 429 that carried no Retry-After hint. TorBox caps NZB creation at 60/hour,
// so a short pause is enough to stop hammering a closed window.
const defaultRateLimitCooldown = 5 * time.Minute

// submitOnce submits every pending job to TorBox.
func (w *Workers) submitOnce(ctx context.Context) error {
	if now := time.Now(); now.Before(w.submitBackoffUntil) {
		return nil // still cooling down from a TorBox rate-limit
	}
	jobs, err := w.store.JobsByState(ctx, job.StatePending)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cooldown := w.submitJob(ctx, j); cooldown > 0 {
			// TorBox is throttling: the job is back in `pending` and the
			// rest of the queue would only earn more 429s. Pause and let a
			// later tick drain it once the window clears.
			w.submitBackoffUntil = time.Now().Add(cooldown)
			return nil
		}
	}
	return nil
}

// submitJob submits one job, retrying transient TorBox errors with backoff.
// On a TorBox rate-limit (429) it leaves the job pending and returns the
// cooldown the submitter should observe; otherwise it returns 0.
func (w *Workers) submitJob(ctx context.Context, j *job.Job) time.Duration {
	log := w.logger.With("job_id", j.ID, "nzb_name", j.NZBName)
	j.State = job.StateSubmitting
	if err := w.store.UpdateJob(ctx, j); err != nil {
		log.Error("marking job submitting", "error", err)
		return 0
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
			if _, limited := torbox.RateLimit(err); limited {
				// A 429 will not clear within the retry budget; stop now and
				// let submitOnce schedule a much longer cooldown instead.
				return backoff.Permanent(err)
			}
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
		if retryAfter, limited := torbox.RateLimit(err); limited {
			// Not a job failure: TorBox is throttling. Revert to pending so a
			// later tick retries this NZB once the rate-limit window clears.
			cooldown := retryAfter
			if cooldown <= 0 {
				cooldown = defaultRateLimitCooldown
			}
			if cooldown > time.Hour {
				cooldown = time.Hour
			}
			j.State = job.StatePending
			if uerr := w.store.UpdateJob(ctx, j); uerr != nil {
				log.Error("reverting rate-limited job to pending", "error", uerr)
			}
			log.Warn("torbox rate-limited submission; pausing submitter",
				"error", err, "cooldown", cooldown.String())
			return cooldown
		}
		j.State = job.StateFailed
		j.FailMessage = "TorBox submission failed: " + err.Error()
		if uerr := w.store.UpdateJob(ctx, j); uerr != nil {
			log.Error("marking job failed", "error", uerr)
		}
		log.Error("submission permanently failed", "error", err)
		return 0
	}

	now := time.Now()
	j.State = job.StateQueued
	j.TorBoxID = int64(result.UsenetDownloadID)
	j.TorBoxHash = result.Hash
	j.SubmittedAt = &now
	if err := w.store.UpdateJob(ctx, j); err != nil {
		log.Error("marking job queued", "error", err)
		return 0
	}
	log.Info("job submitted to torbox", "torbox_id", j.TorBoxID)
	return 0
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
