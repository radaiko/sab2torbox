package worker

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/radaiko/sab2torbox/internal/job"
)

func (w *Workers) healReconcileOnce(_ context.Context) error { return nil }

// healOnce runs the periodic heal cycle: discover library symlinks, detect
// broken ones, and trigger heals for affected jobs.
func (w *Workers) healOnce(ctx context.Context) error {
	w.healLastRun.Store(timeNow().UnixNano())
	if err := w.discoverSymlinks(ctx); err != nil {
		w.logger.Error("heal: discovering symlinks", "error", err)
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
