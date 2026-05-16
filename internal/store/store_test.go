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
