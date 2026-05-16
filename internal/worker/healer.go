package worker

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/radaiko/sab2torbox/internal/job"
	"github.com/radaiko/sab2torbox/internal/torbox"
)

// healReconcileOnce finishes every job in `healing`: once its resubmitted
// download is present on the WebDAV mount, it repoints the broken symlinks
// and returns the job to `imported`. It makes no TorBox call when nothing is
// healing.
func (w *Workers) healReconcileOnce(ctx context.Context) error {
	jobs, err := w.store.JobsByState(ctx, job.StateHealing)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return nil
	}
	list, err := w.tb.ListUsenet(ctx)
	if err != nil {
		return fmt.Errorf("heal: listing torbox usenet: %w", err)
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
			continue // the resubmitted download is not listed yet
		}
		if rec.Failed() {
			w.logger.Warn("heal: torbox download failed",
				"job_id", j.ID, "download_state", rec.DownloadState)
			w.markHealFailed(ctx, j, "TorBox state: "+rec.DownloadState)
			continue
		}
		if !rec.DownloadFinished || !rec.DownloadPresent {
			continue // still downloading
		}
		w.finishHeal(ctx, j, rec)
	}
	return nil
}

// finishHeal repoints every broken symlink of a job to its new release, then
// returns the job to `imported`.
func (w *Workers) finishHeal(ctx context.Context, j *job.Job, rec torbox.UsenetDownload) {
	log := w.logger.With("job_id", j.ID, "torbox_id", j.TorBoxID)
	newReleaseDir, err := w.resolveStoragePath(ctx, rec.Name)
	if err != nil {
		log.Debug("heal: waiting for webdav path", "error", err)
		return // retry next tick
	}
	syms, err := w.store.ListImportedSymlinks(ctx)
	if err != nil {
		log.Error("heal: loading symlinks", "error", err)
		return
	}
	healed, brokenForJob := 0, 0
	for _, sym := range syms {
		if sym.JobID != j.ID || !sym.IsBroken {
			continue
		}
		brokenForJob++
		base := filepath.Base(sym.TargetPath)
		newTarget := filepath.Join(newReleaseDir, base)
		if _, err := os.Stat(newTarget); err != nil {
			match, merr := findBestMatch(newReleaseDir, base)
			if merr != nil {
				log.Warn("heal: no match, leaving symlink broken",
					"symlink", sym.SymlinkPath, "error", merr)
				continue
			}
			newTarget = match
		}
		if err := atomicReplaceSymlink(sym.SymlinkPath, newTarget); err != nil {
			log.Warn("heal: replacing symlink", "symlink", sym.SymlinkPath, "error", err)
			continue
		}
		if err := w.store.UpdateSymlinkTarget(ctx, sym.ID, newTarget); err != nil {
			w.logger.Warn("heal: updating symlink row", "id", sym.ID, "error", err)
		}
		healed++
	}
	if healed == 0 && brokenForJob > 0 {
		// The resubmitted release matched none of the broken symlinks — the
		// heal accomplished nothing. Record it as a failure so it surfaces in
		// /health/heal_failed and the backoff applies, rather than reporting
		// a misleading "healed" with zero repointed links.
		log.Warn("heal: no broken symlink matched the new release; treating as failed",
			"broken", brokenForJob)
		w.markHealFailed(ctx, j, "no broken symlink matched a file in the new release")
		return
	}
	now := timeNow()
	j.State = job.StateImported
	// Keep storage_path in symlink-farm form so discovery still matches it
	// by release name and the deleter's guarded cleanup stays correct.
	j.StoragePath = filepath.Join(w.cfg.SymlinkRoot, j.Category, rec.Name)
	j.ProgressPct = 100
	j.HealCount = 0 // a successful heal clears the consecutive-failure count
	j.LastHealedAt = &now
	j.LastHealError = ""
	if err := w.store.UpdateJob(ctx, j); err != nil {
		log.Error("heal: persisting healed job", "error", err)
		return
	}
	w.emitHealEvent("healed", j, healEventExtra{SymlinksHealed: healed, NewTorBoxID: j.TorBoxID})
	log.Info("heal: completed", "symlinks_healed", healed)
}

// healOnce runs the periodic heal cycle: discover library symlinks, detect
// broken ones, and trigger heals for affected jobs.
func (w *Workers) healOnce(ctx context.Context) error {
	w.healLastRun.Store(timeNow().UnixNano())
	if err := w.discoverSymlinks(ctx); err != nil {
		w.logger.Error("heal: discovering symlinks", "error", err)
	}
	if err := w.detectBrokenSymlinks(ctx); err != nil {
		w.logger.Error("heal: detecting broken symlinks", "error", err)
	}
	if err := w.triggerHeals(ctx); err != nil {
		w.logger.Error("heal: triggering heals", "error", err)
	}
	return nil
}

// discoverSymlinks walks every configured library root and records, in
// imported_symlinks, each symlink that points into the WebDAV mount and whose
// release folder maps to a known job. The hourly re-walk is the source of
// truth — it picks up anything Sonarr renamed or moved.
func (w *Workers) discoverSymlinks(ctx context.Context) error {
	jobs, err := w.store.JobsByState(ctx, job.StateImported, job.StateHealing, job.StateHealFailed)
	if err != nil {
		return err
	}
	byRelease := make(map[string]*job.Job, len(jobs))
	for _, j := range jobs {
		if j.StoragePath != "" {
			byRelease[filepath.Base(j.StoragePath)] = j
		}
	}
	for _, root := range w.cfg.HealLibraryRoots {
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries, keep walking
			}
			if d.Type()&fs.ModeSymlink == 0 {
				return nil
			}
			target, err := os.Readlink(path)
			if err != nil {
				return nil
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			release, ok := releaseUnderRoot(w.cfg.WebDAVMountRoot, target)
			if !ok {
				return nil // points outside the WebDAV mount — not ours
			}
			j, ok := byRelease[release]
			if !ok {
				return nil
			}
			sym := &job.ImportedSymlink{JobID: j.ID, SymlinkPath: path, TargetPath: target}
			if err := w.store.UpsertImportedSymlink(ctx, sym); err != nil {
				w.logger.Warn("heal: recording symlink", "path", path, "error", err)
			}
			return nil
		})
		if walkErr != nil {
			w.logger.Warn("heal: walking library root", "root", root, "error", walkErr)
		}
	}
	return nil
}

// detectBrokenSymlinks verifies every tracked symlink. A symlink whose own
// path is gone (Sonarr renamed/moved it) is dropped — the next discovery
// re-records its new location. A symlink whose target is gone is flagged
// broken for the heal pass.
func (w *Workers) detectBrokenSymlinks(ctx context.Context) error {
	syms, err := w.store.ListImportedSymlinks(ctx)
	if err != nil {
		return err
	}
	jobs, err := w.store.JobsByState(ctx, job.StateImported, job.StateHealing, job.StateHealFailed)
	if err != nil {
		return err
	}
	eligible := make(map[int64]bool, len(jobs))
	for _, j := range jobs {
		eligible[j.ID] = true
	}
	now := timeNow()
	for _, sym := range syms {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !eligible[sym.JobID] {
			// The owning job is no longer heal-relevant — stop tracking it so
			// its stale rows don't inflate the broken-symlink count.
			if derr := w.store.DeleteImportedSymlink(ctx, sym.ID); derr != nil {
				w.logger.Warn("heal: removing symlink for stale job", "id", sym.ID, "error", derr)
			}
			continue
		}
		if _, err := os.Lstat(sym.SymlinkPath); err != nil {
			if derr := w.store.DeleteImportedSymlink(ctx, sym.ID); derr != nil {
				w.logger.Warn("heal: removing stale symlink row", "id", sym.ID, "error", derr)
			}
			continue
		}
		broken := false
		if _, err := os.Stat(sym.SymlinkPath); err != nil {
			broken = true // Lstat ok but Stat fails -> the target is gone
		}
		if err := w.store.SetSymlinkVerified(ctx, sym.ID, broken, now); err != nil {
			w.logger.Warn("heal: updating symlink state", "id", sym.ID, "error", err)
		}
		if broken {
			w.logger.Warn("heal: broken symlink",
				"path", sym.SymlinkPath, "target", sym.TargetPath)
		}
	}
	return nil
}

// releaseUnderRoot returns the first path component of target beneath
// webdavRoot — the TorBox release folder name — or false if target is not
// under webdavRoot.
func releaseUnderRoot(webdavRoot, target string) (string, bool) {
	rel, err := filepath.Rel(webdavRoot, target)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 0 || parts[0] == "" {
		return "", false
	}
	return parts[0], true
}

// triggerHeals resubmits the stored NZB for every job that has at least one
// broken symlink and is eligible (not already healing, attempts left, past
// its backoff). The job transitions to `healing`; healReconcileOnce finishes it.
func (w *Workers) triggerHeals(ctx context.Context) error {
	syms, err := w.store.ListImportedSymlinks(ctx)
	if err != nil {
		return err
	}
	brokenByJob := make(map[int64]int)
	for _, sym := range syms {
		if sym.IsBroken {
			brokenByJob[sym.JobID]++
		}
	}
	for jobID, count := range brokenByJob {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		j, err := w.store.GetJob(ctx, jobID)
		if err != nil {
			continue
		}
		if j.State != job.StateImported && j.State != job.StateHealFailed {
			continue // healing already, or deleted — not eligible
		}
		if int(j.HealCount) >= w.cfg.HealMaxAttempts {
			continue // exhausted — manual intervention needed
		}
		if j.LastHealedAt != nil {
			backoff := healBackoff(w.cfg.HealBackoffInitial, j.HealCount)
			if timeNow().Sub(*j.LastHealedAt) < backoff {
				continue // still in backoff
			}
		}
		if w.cfg.HealDryRun {
			w.logger.Info("heal: dry-run, would heal",
				"job_id", j.ID, "broken_symlinks", count)
			continue
		}
		w.startHeal(ctx, j, count)
	}
	return nil
}

// startHeal resubmits a job's stored NZB to TorBox and moves it to `healing`.
func (w *Workers) startHeal(ctx context.Context, j *job.Job, brokenCount int) {
	log := w.logger.With("job_id", j.ID, "nzb_name", j.NZBName)
	w.emitHealEvent("detected", j, healEventExtra{})
	if len(j.NZBContent) == 0 && j.NZBURL == "" {
		log.Error("heal: no stored NZB to resubmit")
		w.markHealFailed(ctx, j, "no stored NZB content")
		return
	}
	res, err := w.tb.CreateUsenetDownload(ctx, torbox.CreateRequest{
		NZBContent: j.NZBContent,
		NZBName:    j.NZBName + ".nzb",
		Link:       j.NZBURL,
	})
	if err != nil {
		log.Warn("heal: resubmission failed", "error", err)
		w.markHealFailed(ctx, j, "resubmission failed: "+err.Error())
		return
	}
	j.State = job.StateHealing
	j.TorBoxID = int64(res.UsenetDownloadID)
	j.TorBoxHash = res.Hash
	j.LastHealError = ""
	if err := w.store.UpdateJob(ctx, j); err != nil {
		log.Error("heal: persisting healing state, marking failed to avoid a duplicate resubmit",
			"error", err)
		w.markHealFailed(ctx, j, "persisting healing state failed: "+err.Error())
		return
	}
	w.emitHealEvent("healing", j, healEventExtra{NewTorBoxID: j.TorBoxID})
	log.Info("heal: resubmitted to torbox",
		"torbox_id", j.TorBoxID, "broken_symlinks", brokenCount)
}

// markHealFailed records a failed heal attempt and applies backoff via
// last_healed_at.
func (w *Workers) markHealFailed(ctx context.Context, j *job.Job, msg string) {
	now := timeNow()
	j.State = job.StateHealFailed
	j.HealCount++
	j.LastHealedAt = &now
	j.LastHealError = msg
	if err := w.store.UpdateJob(ctx, j); err != nil {
		w.logger.Error("heal: persisting heal_failed", "job_id", j.ID, "error", err)
	}
	w.emitHealEvent("failed", j, healEventExtra{Error: msg})
}

// healBackoff is exponential: HealBackoffInitial doubled once per prior attempt.
func healBackoff(initial time.Duration, count int64) time.Duration {
	d := initial
	for i := int64(0); i < count; i++ {
		d *= 2
	}
	return d
}
