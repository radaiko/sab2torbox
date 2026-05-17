package worker

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// EnsureCategoryDirs creates <symlinkRoot>/<category>/ for every category, so
// Sonarr/Radarr health checks see their drop zone before any download lands.
func EnsureCategoryDirs(symlinkRoot string, categories []string) error {
	for _, c := range categories {
		dir := filepath.Join(symlinkRoot, c)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating category dir %q: %w", dir, err)
		}
	}
	return nil
}

// buildSymlinkFarm mirrors every file of a completed release (rooted at
// sourceDir on the WebDAV mount) as a symlink under
// <symlinkRoot>/<category>/<release>/. It returns that directory and the
// number of files the release contains. A files count of zero means TorBox's
// WebDAV listing has surfaced the release folder but not its contents yet;
// the caller must treat the release as not ready rather than completed.
// Symlink targets are absolute so any container that mounts the WebDAV path
// can resolve them. It is idempotent: an existing link is left untouched.
func buildSymlinkFarm(symlinkRoot, category, release, sourceDir string) (dest string, files int, err error) {
	dest = filepath.Join(symlinkRoot, category, release)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", 0, fmt.Errorf("creating release dir %q: %w", dest, err)
	}
	walkErr := filepath.WalkDir(sourceDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		files++
		if _, err := os.Lstat(target); err == nil {
			return nil // already linked
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if err := os.Symlink(abs, target); err != nil {
			return fmt.Errorf("linking %q: %w", target, err)
		}
		return nil
	})
	if walkErr != nil {
		return "", 0, fmt.Errorf("building symlink farm for %q: %w", release, walkErr)
	}
	return dest, files, nil
}

// removeSymlinkDir removes a per-release symlink directory, but only when it
// genuinely sits inside symlinkRoot — a guard so an unexpected storage_path
// can never delete something outside the farm.
func removeSymlinkDir(symlinkRoot, dir string) error {
	if !isWithin(symlinkRoot, dir) {
		return fmt.Errorf("refusing to remove %q: not under symlink root %q", dir, symlinkRoot)
	}
	return os.RemoveAll(dir)
}

// isWithin reports whether path is a descendant of root.
func isWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// classifyReleaseDir inspects a per-release symlink directory. empty is true
// when it holds no files (Sonarr has moved the symlink into its library);
// allBroken is true when every file is a symlink whose target is gone.
func classifyReleaseDir(dir string) (empty, allBroken bool, err error) {
	var files, broken int
	werr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		files++
		if _, statErr := os.Stat(path); statErr != nil { // Stat follows symlinks
			broken++
		}
		return nil
	})
	if werr != nil {
		return false, false, werr
	}
	return files == 0, files > 0 && broken == files, nil
}

// atomicReplaceSymlink repoints linkPath at newTarget without ever leaving
// linkPath absent: it creates a temp symlink and renames it over linkPath.
// rename(2) is atomic on POSIX, so a process with the file open keeps reading.
func atomicReplaceSymlink(linkPath, newTarget string) error {
	tmp := linkPath + ".heal-tmp"
	_ = os.Remove(tmp) // clear any stale temp link from a crashed run
	if err := os.Symlink(newTarget, tmp); err != nil {
		return fmt.Errorf("creating temp symlink: %w", err)
	}
	if err := os.Rename(tmp, linkPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("atomically replacing symlink: %w", err)
	}
	return nil
}

// lcp returns the length of the longest common prefix of a and b.
func lcp(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// findBestMatch locates the file in dir that most likely corresponds to
// oldBasename, for the case where a re-submitted release names its files
// slightly differently. It never guesses wildly: if nothing plausibly
// matches it returns an error so the caller leaves the old symlink alone.
func findBestMatch(dir, oldBasename string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reading %q: %w", dir, err)
	}
	// 1. Exact match, case-insensitive.
	for _, e := range entries {
		if strings.EqualFold(e.Name(), oldBasename) {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	// 2. Same extension, longest common prefix (case-insensitive).
	oldExt := strings.ToLower(filepath.Ext(oldBasename))
	lowerOld := strings.ToLower(oldBasename)
	var best string
	bestScore := 0
	for _, e := range entries {
		if strings.ToLower(filepath.Ext(e.Name())) != oldExt {
			continue
		}
		if score := lcp(strings.ToLower(e.Name()), lowerOld); score > bestScore {
			best, bestScore = filepath.Join(dir, e.Name()), score
		}
	}
	if best != "" && bestScore > len(oldBasename)/2 {
		return best, nil
	}
	// No confident match — return an error so finishHeal leaves the old
	// (broken) symlink in place. Guessing by "lone video file" could repoint
	// a library entry at different content (a PROPER, a different edition).
	return "", fmt.Errorf("no confident match for %q in %q", oldBasename, dir)
}
