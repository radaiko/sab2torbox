package worker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureCategoryDirs(t *testing.T) {
	root := t.TempDir()
	if err := EnsureCategoryDirs(root, []string{"sonarr", "radarr"}); err != nil {
		t.Fatalf("EnsureCategoryDirs: %v", err)
	}
	for _, c := range []string{"sonarr", "radarr"} {
		if info, err := os.Stat(filepath.Join(root, c)); err != nil || !info.IsDir() {
			t.Errorf("category dir %s missing", c)
		}
	}
}

func TestBuildSymlinkFarm(t *testing.T) {
	src := t.TempDir()
	srcFile := filepath.Join(src, "the.rookie.s08e01.mkv")
	if err := os.WriteFile(srcFile, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()

	dest, files, err := buildSymlinkFarm(root, "sonarr-stream", "The.Rookie.S08E01", src)
	if err != nil {
		t.Fatalf("buildSymlinkFarm: %v", err)
	}
	if files != 1 {
		t.Errorf("files: got %d want 1", files)
	}
	want := filepath.Join(root, "sonarr-stream", "The.Rookie.S08E01")
	if dest != want {
		t.Errorf("dest: got %q want %q", dest, want)
	}
	link := filepath.Join(dest, "the.rookie.s08e01.mkv")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != srcFile {
		t.Errorf("symlink target: got %q want %q", target, srcFile)
	}
	if b, err := os.ReadFile(link); err != nil || string(b) != "video" {
		t.Errorf("reading through symlink: %q err=%v", b, err)
	}
	// Idempotent: a second build over an existing farm must not error and
	// must still report the file count.
	if _, files, err := buildSymlinkFarm(root, "sonarr-stream", "The.Rookie.S08E01", src); err != nil || files != 1 {
		t.Errorf("second buildSymlinkFarm: files=%d err=%v", files, err)
	}
}

func TestBuildSymlinkFarmEmptySource(t *testing.T) {
	// TorBox's WebDAV can list a release folder before its files appear.
	src := t.TempDir() // exists, but holds no files
	root := t.TempDir()
	_, files, err := buildSymlinkFarm(root, "cat", "Rel", src)
	if err != nil {
		t.Fatalf("buildSymlinkFarm: %v", err)
	}
	if files != 0 {
		t.Errorf("an empty release must report 0 files, got %d", files)
	}
}

func TestBuildSymlinkFarmNested(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "Sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "Sub", "ep.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dest, _, err := buildSymlinkFarm(root, "cat", "Rel", src)
	if err != nil {
		t.Fatalf("buildSymlinkFarm: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "Sub", "ep.mkv")); err != nil {
		t.Errorf("nested file not linked: %v", err)
	}
}

func TestRemoveSymlinkDirGuard(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "cat", "Rel")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeSymlinkDir(root, inside); err != nil {
		t.Fatalf("removeSymlinkDir inside: %v", err)
	}
	if _, err := os.Stat(inside); err == nil {
		t.Error("dir should be removed")
	}
	if err := removeSymlinkDir(root, t.TempDir()); err == nil {
		t.Error("removeSymlinkDir must refuse a path outside the root")
	}
	if err := removeSymlinkDir(root, root); err == nil {
		t.Error("removeSymlinkDir must refuse the root itself")
	}
}

func TestClassifyReleaseDir(t *testing.T) {
	empty := t.TempDir()
	if e, _, err := classifyReleaseDir(empty); err != nil || !e {
		t.Errorf("empty dir: empty=%v err=%v", e, err)
	}

	src := t.TempDir()
	good := filepath.Join(src, "f.mkv")
	if err := os.WriteFile(good, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	live := t.TempDir()
	if err := os.Symlink(good, filepath.Join(live, "f.mkv")); err != nil {
		t.Fatal(err)
	}
	if e, b, _ := classifyReleaseDir(live); e || b {
		t.Errorf("dir with a live symlink: empty=%v allBroken=%v", e, b)
	}

	broken := t.TempDir()
	if err := os.Symlink(filepath.Join(src, "gone.mkv"), filepath.Join(broken, "gone.mkv")); err != nil {
		t.Fatal(err)
	}
	if _, b, _ := classifyReleaseDir(broken); !b {
		t.Error("dir with only a dangling symlink should classify as allBroken")
	}
}

func TestIsWithin(t *testing.T) {
	if !isWithin("/a/b", "/a/b/c") {
		t.Error("/a/b/c is within /a/b")
	}
	if isWithin("/a/b", "/a/b") {
		t.Error("the root itself is not 'within'")
	}
	if isWithin("/a/b", "/a/x") {
		t.Error("/a/x is not within /a/b")
	}
}

func TestAtomicReplaceSymlink(t *testing.T) {
	dir := t.TempDir()
	oldTarget := filepath.Join(dir, "old.mkv")
	newTarget := filepath.Join(dir, "new.mkv")
	if err := os.WriteFile(oldTarget, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newTarget, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.mkv")
	if err := os.Symlink(oldTarget, link); err != nil {
		t.Fatal(err)
	}
	if err := atomicReplaceSymlink(link, newTarget); err != nil {
		t.Fatalf("atomicReplaceSymlink: %v", err)
	}
	got, _ := os.Readlink(link)
	if got != newTarget {
		t.Errorf("link target: got %q want %q", got, newTarget)
	}
	if b, _ := os.ReadFile(link); string(b) != "new" {
		t.Errorf("content through link: %q", b)
	}
}

func TestLCP(t *testing.T) {
	if lcp("abcdef", "abcxyz") != 3 {
		t.Errorf("lcp = %d, want 3", lcp("abcdef", "abcxyz"))
	}
	if lcp("", "x") != 0 {
		t.Error("lcp with empty string must be 0")
	}
}

func TestFindBestMatch(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"the.rookie.s08e01.GERMAN.mkv", "sample.mkv", "info.nfo"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Case-insensitive prefix match wins over the unrelated sample.
	got, err := findBestMatch(dir, "The.Rookie.S08E01.german.mkv")
	if err != nil {
		t.Fatalf("findBestMatch: %v", err)
	}
	if filepath.Base(got) != "the.rookie.s08e01.GERMAN.mkv" {
		t.Errorf("match: got %q", got)
	}

	// A differently-named lone file is NOT a confident match — findBestMatch
	// must error rather than guess and risk relinking to different content.
	solo := t.TempDir()
	if err := os.WriteFile(filepath.Join(solo, "totally.different.name.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := findBestMatch(solo, "original.mkv"); err == nil {
		t.Error("a name with no plausible match must not be guessed at")
	}

	// No plausible match -> error.
	empty := t.TempDir()
	if _, err := findBestMatch(empty, "x.mkv"); err == nil {
		t.Error("expected error when nothing matches")
	}
}
