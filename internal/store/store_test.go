package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/radaiko/sab2torbox/internal/job"
)

// newTestStore opens a fresh migrated store in a temp directory.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenRunsMigrations(t *testing.T) {
	s := newTestStore(t)
	var name string
	err := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='jobs'`).Scan(&name)
	if err != nil {
		t.Fatalf("jobs table not created: %v", err)
	}
}

func TestCreateGetJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, err := s.CreateJob(ctx, &job.Job{
		State: job.StatePending, Category: "sonarr",
		NZBName: "Show.S01E01", NZBSHA256: "abc",
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	got, err := s.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.NZBName != "Show.S01E01" || got.State != job.StatePending {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestUpdateJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, _ := s.CreateJob(ctx, &job.Job{State: job.StatePending, Category: "sonarr", NZBName: "x"})
	j, _ := s.GetJob(ctx, id)
	j.State = job.StateQueued
	j.TorBoxID = 99
	j.ProgressPct = 50
	j.ETASeconds = 300
	if err := s.UpdateJob(ctx, j); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	got, _ := s.GetJob(ctx, id)
	if got.State != job.StateQueued || got.TorBoxID != 99 || got.ProgressPct != 50 {
		t.Errorf("update not persisted: %+v", got)
	}
	if got.ETASeconds != 300 {
		t.Errorf("eta_seconds not persisted: got %d", got.ETASeconds)
	}
}

func TestJobsByStateAndFinders(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.CreateJob(ctx, &job.Job{State: job.StatePending, Category: "sonarr", NZBName: "a", NZBSHA256: "sha-a"})
	s.CreateJob(ctx, &job.Job{State: job.StateQueued, Category: "sonarr", NZBName: "b", NZBURL: "http://u/b"})

	pending, err := s.JobsByState(ctx, job.StatePending)
	if err != nil || len(pending) != 1 {
		t.Fatalf("JobsByState pending: len=%d err=%v", len(pending), err)
	}
	bySha, _ := s.FindBySHA256(ctx, "sha-a", "sonarr")
	if bySha == nil || bySha.NZBName != "a" {
		t.Errorf("FindBySHA256 miss: %+v", bySha)
	}
	if miss, _ := s.FindBySHA256(ctx, "sha-a", "radarr"); miss != nil {
		t.Error("FindBySHA256 must be category-scoped")
	}
	byURL, _ := s.FindByURL(ctx, "http://u/b", "sonarr")
	if byURL == nil || byURL.NZBName != "b" {
		t.Errorf("FindByURL miss: %+v", byURL)
	}
}

func TestDeleteJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, _ := s.CreateJob(ctx, &job.Job{State: job.StatePending, Category: "c", NZBName: "x"})
	if err := s.DeleteJob(ctx, id); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if _, err := s.GetJob(ctx, id); err == nil {
		t.Error("expected GetJob to fail after delete")
	}
}

func TestActiveStoragePaths(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mk := func(state job.State, sp string) {
		id, _ := s.CreateJob(ctx, &job.Job{State: state, Category: "c", NZBName: "n"})
		j, _ := s.GetJob(ctx, id)
		j.StoragePath = sp
		if err := s.UpdateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	mk(job.StateCompleted, "/farm/a")
	mk(job.StateImported, "/farm/b")
	mk(job.StateDeleted, "/farm/c") // terminal -> excluded
	mk(job.StateFailed, "/farm/d")  // terminal -> excluded
	mk(job.StateDownloading, "")    // no storage path -> excluded

	paths, err := s.ActiveStoragePaths(ctx)
	if err != nil {
		t.Fatalf("ActiveStoragePaths: %v", err)
	}
	if len(paths) != 2 {
		t.Errorf("got %v, want 2 active paths (/farm/a, /farm/b)", paths)
	}
}

func TestReapImported(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, _ := s.CreateJob(ctx, &job.Job{State: job.StateImported, Category: "c", NZBName: "old"})
	if _, err := s.Exec(ctx, `UPDATE jobs SET updated_at=? WHERE id=?`,
		time.Now().Add(-48*time.Hour), id); err != nil {
		t.Fatal(err)
	}
	n, err := s.ReapImported(ctx, time.Now().Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("ReapImported: n=%d err=%v", n, err)
	}
}

func TestHealColumnsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, _ := s.CreateJob(ctx, &job.Job{State: job.StateImported, Category: "c", NZBName: "n"})
	j, _ := s.GetJob(ctx, id)
	now := time.Now().UTC().Truncate(time.Second)
	j.HealCount = 2
	j.LastHealedAt = &now
	j.LastHealError = "boom"
	if err := s.UpdateJob(ctx, j); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	got, _ := s.GetJob(ctx, id)
	if got.HealCount != 2 || got.LastHealError != "boom" || got.LastHealedAt == nil {
		t.Errorf("heal columns not persisted: %+v", got)
	}
}

func TestImportedSymlinkCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	jobID, _ := s.CreateJob(ctx, &job.Job{State: job.StateImported, Category: "c", NZBName: "n"})

	sym := &job.ImportedSymlink{JobID: jobID, SymlinkPath: "/lib/a.mkv", TargetPath: "/mnt/torbox/Rel/a.mkv"}
	if err := s.UpsertImportedSymlink(ctx, sym); err != nil {
		t.Fatalf("UpsertImportedSymlink: %v", err)
	}
	// Upsert again with a new target — must update, not duplicate.
	sym.TargetPath = "/mnt/torbox/Rel2/a.mkv"
	if err := s.UpsertImportedSymlink(ctx, sym); err != nil {
		t.Fatalf("UpsertImportedSymlink (update): %v", err)
	}
	list, err := s.ListImportedSymlinks(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListImportedSymlinks: len=%d err=%v", len(list), err)
	}
	if list[0].TargetPath != "/mnt/torbox/Rel2/a.mkv" {
		t.Errorf("target not updated: %q", list[0].TargetPath)
	}

	id := list[0].ID
	if err := s.SetSymlinkVerified(ctx, id, true, time.Now()); err != nil {
		t.Fatalf("SetSymlinkVerified: %v", err)
	}
	if list, _ = s.ListImportedSymlinks(ctx); !list[0].IsBroken {
		t.Error("symlink should be marked broken")
	}
	if err := s.UpdateSymlinkTarget(ctx, id, "/mnt/torbox/Rel3/a.mkv"); err != nil {
		t.Fatalf("UpdateSymlinkTarget: %v", err)
	}
	if list, _ = s.ListImportedSymlinks(ctx); list[0].IsBroken || list[0].TargetPath != "/mnt/torbox/Rel3/a.mkv" {
		t.Errorf("UpdateSymlinkTarget should clear is_broken and set target: %+v", list[0])
	}

	tracked, broken, err := s.SymlinkCounts(ctx)
	if err != nil || tracked != 1 || broken != 0 {
		t.Errorf("SymlinkCounts: tracked=%d broken=%d err=%v", tracked, broken, err)
	}
	if err := s.DeleteImportedSymlink(ctx, id); err != nil {
		t.Fatalf("DeleteImportedSymlink: %v", err)
	}
	if list, _ = s.ListImportedSymlinks(ctx); len(list) != 0 {
		t.Error("symlink row should be deleted")
	}
}

func TestCountJobsByState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.CreateJob(ctx, &job.Job{State: job.StateHealing, Category: "c", NZBName: "a"})
	s.CreateJob(ctx, &job.Job{State: job.StateHealing, Category: "c", NZBName: "b"})
	s.CreateJob(ctx, &job.Job{State: job.StateHealFailed, Category: "c", NZBName: "d"})
	n, err := s.CountJobsByState(ctx, job.StateHealing)
	if err != nil || n != 2 {
		t.Errorf("CountJobsByState healing: n=%d err=%v", n, err)
	}
}

func TestDeleteImportedSymlinksByJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	jobA, _ := s.CreateJob(ctx, &job.Job{State: job.StateImported, Category: "c", NZBName: "a"})
	jobB, _ := s.CreateJob(ctx, &job.Job{State: job.StateImported, Category: "c", NZBName: "b"})
	s.UpsertImportedSymlink(ctx, &job.ImportedSymlink{JobID: jobA, SymlinkPath: "/l/a1", TargetPath: "/t/a1"})
	s.UpsertImportedSymlink(ctx, &job.ImportedSymlink{JobID: jobA, SymlinkPath: "/l/a2", TargetPath: "/t/a2"})
	s.UpsertImportedSymlink(ctx, &job.ImportedSymlink{JobID: jobB, SymlinkPath: "/l/b1", TargetPath: "/t/b1"})

	if err := s.DeleteImportedSymlinksByJob(ctx, jobA); err != nil {
		t.Fatalf("DeleteImportedSymlinksByJob: %v", err)
	}
	list, _ := s.ListImportedSymlinks(ctx)
	if len(list) != 1 || list[0].JobID != jobB {
		t.Errorf("only job B's symlink should remain, got %+v", list)
	}
}
