package worker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/radaiko/sab2torbox/internal/job"
)

// importedTTL is how long an imported job is kept before the reaper drops it.
const importedTTL = 24 * time.Hour

// reapOnce deletes imported jobs older than importedTTL (a safety net for
// downloads Sonarr never deleted via the SAB API) and, in symlink mode,
// sweeps stale per-release directories out of the symlink farm.
func (w *Workers) reapOnce(ctx context.Context) error {
	w.detectImports(ctx)
	n, err := w.store.ReapImported(ctx, time.Now().Add(-importedTTL))
	if err != nil {
		return err
	}
	if n > 0 {
		w.logger.Info("reaped imported jobs", "count", n)
	}
	w.sweepSymlinkFarm(ctx)
	return nil
}

// detectImports advances completed jobs to `imported` once Sonarr has taken
// their files. In symlink mode Sonarr imports a release by moving each
// symlink out of the per-release farm directory, so a release directory that
// is empty (or has already been swept away) means the import is done. This is
// the honest import signal: it lets reapImported eventually drop the row,
// hands the job to the healer, and stops handleHistory from advertising a
// release Sonarr has already imported.
func (w *Workers) detectImports(ctx context.Context) {
	jobs, err := w.store.JobsByState(ctx, job.StateCompleted)
	if err != nil {
		w.logger.Error("loading completed jobs", "error", err)
		return
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return
		}
		if j.StoragePath == "" {
			continue
		}
		empty, _, cerr := classifyReleaseDir(j.StoragePath)
		switch {
		case errors.Is(cerr, fs.ErrNotExist):
			empty = true // dir already swept away — fully imported
		case cerr != nil:
			w.logger.Warn("checking release dir for import",
				"dir", j.StoragePath, "error", cerr)
			continue
		}
		if !empty {
			continue // files still in the farm — Sonarr has not imported yet
		}
		j.State = job.StateImported
		if err := w.store.UpdateJob(ctx, j); err != nil {
			w.logger.Error("marking job imported", "job_id", j.ID, "error", err)
			continue
		}
		w.logger.Info("job imported by sonarr",
			"job_id", j.ID, "storage_path", j.StoragePath)
	}
}

// sweepSymlinkFarm removes per-release symlink directories that are no longer
// needed: empty ones (Sonarr moved the symlink into its library) and fully
// broken ones (every target gone) whose job is no longer active. It never
// removes a category directory or the symlink root itself.
func (w *Workers) sweepSymlinkFarm(ctx context.Context) {
	active, err := w.store.ActiveStoragePaths(ctx)
	if err != nil {
		w.logger.Error("loading active storage paths", "error", err)
		return
	}
	activeSet := make(map[string]bool, len(active))
	for _, p := range active {
		activeSet[p] = true
	}
	for _, category := range w.cfg.Categories {
		catDir := filepath.Join(w.cfg.SymlinkRoot, category)
		entries, err := os.ReadDir(catDir)
		if err != nil {
			continue // category dir not created yet — nothing to sweep
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			releaseDir := filepath.Join(catDir, e.Name())
			empty, allBroken, cerr := classifyReleaseDir(releaseDir)
			if cerr != nil {
				w.logger.Warn("scanning symlink release dir", "dir", releaseDir, "error", cerr)
				continue
			}
			switch {
			case empty:
				if err := os.Remove(releaseDir); err != nil {
					w.logger.Warn("removing empty symlink dir", "dir", releaseDir, "error", err)
				}
			case allBroken && !activeSet[releaseDir]:
				w.logger.Warn("removing orphaned symlink dir (all targets gone)",
					"dir", releaseDir)
				if err := os.RemoveAll(releaseDir); err != nil {
					w.logger.Warn("removing orphaned symlink dir", "dir", releaseDir, "error", err)
				}
			}
		}
	}
}
