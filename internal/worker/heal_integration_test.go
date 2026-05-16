package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/radaiko/sab2torbox/internal/config"
	"github.com/radaiko/sab2torbox/internal/job"
	"github.com/radaiko/sab2torbox/internal/store"
	"github.com/radaiko/sab2torbox/internal/torbox"
)

// TestEndToEndRotationHeal simulates: a release is imported, TorBox rotates it
// out (target deleted), the healer resubmits, and the library symlink is
// repointed at the new release folder.
func TestEndToEndRotationHeal(t *testing.T) {
	mountRoot := t.TempDir()
	symlinkRoot := t.TempDir()
	libRoot := t.TempDir()
	newRelease := "Rel.Rotated"

	// Mock TorBox: create returns a new id; mylist reports it finished.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/api/usenet/createusenetdownload":
			w.Write([]byte(`{"success":true,"data":{"usenetdownload_id":9100,"hash":"h"}}`))
		case "/v1/api/usenet/mylist":
			resp := map[string]any{"success": true, "data": []any{map[string]any{
				"id": 9100, "name": newRelease, "progress": 1,
				"download_finished": true, "download_present": true,
			}}}
			json.NewEncoder(w).Encode(resp)
		}
	}))
	defer srv.Close()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "heal.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		WebDAVMountRoot: mountRoot, SymlinkRoot: symlinkRoot,
		PollInterval: time.Millisecond, HealEnabled: true,
		HealLibraryRoots: []string{libRoot}, HealInterval: time.Millisecond,
		HealMaxAttempts: 3, HealBackoffInitial: time.Millisecond,
	}
	tb := torbox.NewWithBaseURL("tok", srv.URL+"/v1/api")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := New(st, tb, cfg, logger)
	ctx := context.Background()

	// An imported job; storage_path in symlink-farm form names the old release.
	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StateImported, Category: "sonarr", NZBName: newRelease,
		NZBContent: []byte("<nzb/>"),
	})
	j, _ := st.GetJob(ctx, id)
	j.StoragePath = filepath.Join(symlinkRoot, "sonarr", newRelease)
	st.UpdateJob(ctx, j)

	// The library symlink Sonarr left behind, pointing at the (soon gone) target.
	oldTarget := filepath.Join(mountRoot, newRelease, "ep.mkv")
	os.MkdirAll(filepath.Dir(oldTarget), 0o755)
	os.WriteFile(oldTarget, []byte("v"), 0o644)
	link := filepath.Join(libRoot, "Show", "ep.mkv")
	os.MkdirAll(filepath.Dir(link), 0o755)
	os.Symlink(oldTarget, link)

	// Heal cycle 1: discover the symlink (target still present, not broken).
	if err := w.healOnce(ctx); err != nil {
		t.Fatalf("healOnce 1: %v", err)
	}

	// TorBox rotates the release out: the target disappears.
	os.RemoveAll(filepath.Join(mountRoot, newRelease))

	// Heal cycle 2: detect broken + resubmit -> job goes `healing`.
	if err := w.healOnce(ctx); err != nil {
		t.Fatalf("healOnce 2: %v", err)
	}
	if got, _ := st.GetJob(ctx, id); got.State != job.StateHealing {
		t.Fatalf("after resubmit: state %s, want healing", got.State)
	}

	// The new release folder appears on the WebDAV mount.
	newDir := filepath.Join(mountRoot, newRelease)
	os.MkdirAll(newDir, 0o755)
	os.WriteFile(filepath.Join(newDir, "ep.mkv"), []byte("v2"), 0o644)

	// Reconcile: finish the heal.
	if err := w.healReconcileOnce(ctx); err != nil {
		t.Fatalf("healReconcileOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateImported || got.HealCount != 0 {
		t.Fatalf("after heal: state=%s heal_count=%d", got.State, got.HealCount)
	}
	target, _ := os.Readlink(link)
	if target != filepath.Join(newDir, "ep.mkv") {
		t.Errorf("symlink not healed: %q", target)
	}
	if b, _ := os.ReadFile(link); string(b) != "v2" {
		t.Errorf("healed symlink reads stale content: %q", b)
	}
}

// TestEndToEndHealResubmitFailure verifies a failed resubmission backs off.
func TestEndToEndHealResubmitFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"success":false,"detail":"nope"}`))
	}))
	defer srv.Close()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "heal2.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		WebDAVMountRoot: t.TempDir(), SymlinkRoot: t.TempDir(),
		PollInterval: time.Millisecond, HealEnabled: true,
		HealLibraryRoots: []string{t.TempDir()}, HealInterval: time.Millisecond,
		HealMaxAttempts: 3, HealBackoffInitial: time.Hour,
	}
	tb := torbox.NewWithBaseURL("tok", srv.URL+"/v1/api")
	w := New(st, tb, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StateImported, Category: "c", NZBName: "n", NZBContent: []byte("x"),
	})
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: id, SymlinkPath: "/lib/x.mkv", TargetPath: "/mnt/torbox/n/x.mkv",
	})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())

	if err := w.triggerHeals(ctx); err != nil {
		t.Fatalf("triggerHeals: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateHealFailed {
		t.Errorf("state: got %s want heal_failed", got.State)
	}
	if got.HealCount != 1 || got.LastHealError == "" {
		t.Errorf("failed heal not recorded: count=%d err=%q", got.HealCount, got.LastHealError)
	}
}
