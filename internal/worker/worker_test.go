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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/radaiko/sab2torbox/internal/config"
	"github.com/radaiko/sab2torbox/internal/job"
	"github.com/radaiko/sab2torbox/internal/store"
	"github.com/radaiko/sab2torbox/internal/torbox"
)

// fakeTorBox is an in-memory TorBoxAPI for tests.
type fakeTorBox struct {
	mu          sync.Mutex
	created     []torbox.CreateRequest
	createErr   error
	createCalls int
	nextID      int64
	list        []torbox.UsenetDownload
	controls    []string
	controlErr  error
}

func (f *fakeTorBox) CreateUsenetDownload(_ context.Context, r torbox.CreateRequest) (*torbox.CreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, r)
	f.nextID++
	return &torbox.CreateResult{UsenetDownloadID: torbox.FlexInt(f.nextID), Hash: "h"}, nil
}

func (f *fakeTorBox) ListUsenet(context.Context) ([]torbox.UsenetDownload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list, nil
}

func (f *fakeTorBox) ControlUsenet(_ context.Context, id int64, op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.controls = append(f.controls, op)
	return f.controlErr
}

func (f *fakeTorBox) Ping(context.Context) error { return nil }

func testWorkers(t *testing.T, tb TorBoxAPI) (*Workers, *store.Store, *config.Config) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		WebDAVMountRoot: t.TempDir(), WebDAVUsenetSubpath: "usenet",
		SymlinkRoot:  t.TempDir(),
		PollInterval: 10 * time.Millisecond,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(st, tb, cfg, logger), st, cfg
}

func TestSubmitterSubmitsPendingJob(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StatePending, Category: "sonarr", NZBName: "Rel",
		NZBContent: []byte("<nzb/>"),
	})

	if err := w.submitOnce(ctx); err != nil {
		t.Fatalf("submitOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateQueued {
		t.Errorf("state: got %s want queued", got.State)
	}
	if got.TorBoxID == 0 {
		t.Error("torbox id not stored")
	}
	if len(fake.created) != 1 {
		t.Errorf("expected 1 submission, got %d", len(fake.created))
	}
}

func TestSubmitterPermanentFailureMarksFailed(t *testing.T) {
	fake := &fakeTorBox{createErr: &torbox.APIError{Status: 400, Detail: "bad nzb"}}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StatePending, Category: "sonarr", NZBName: "Rel", NZBContent: []byte("x"),
	})
	_ = w.submitOnce(ctx)
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateFailed {
		t.Errorf("state: got %s want failed", got.State)
	}
	if got.FailMessage == "" {
		t.Error("fail_message should be populated")
	}
}

func TestSubmitterRateLimitKeepsJobPending(t *testing.T) {
	fake := &fakeTorBox{createErr: &torbox.APIError{Status: 429, Detail: "60 per 1 hour"}}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StatePending, Category: "sonarr", NZBName: "Rel", NZBContent: []byte("x"),
	})

	if err := w.submitOnce(ctx); err != nil {
		t.Fatalf("submitOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StatePending {
		t.Fatalf("a rate-limited job must stay pending, not %s", got.State)
	}
	if got.FailMessage != "" {
		t.Errorf("a rate-limit is not a failure: fail_message=%q", got.FailMessage)
	}
	if !w.submitBackoffUntil.After(time.Now()) {
		t.Fatal("submitter should enter a cooldown after a 429")
	}

	// A tick inside the cooldown window must not call TorBox again.
	before := fake.createCalls
	if err := w.submitOnce(ctx); err != nil {
		t.Fatalf("submitOnce during cooldown: %v", err)
	}
	if fake.createCalls != before {
		t.Errorf("submitter hit TorBox during cooldown: %d extra call(s)", fake.createCalls-before)
	}
}

func TestSubmitterRateLimitHonorsRetryAfter(t *testing.T) {
	fake := &fakeTorBox{createErr: &torbox.APIError{
		Status: 429, Detail: "slow down", RetryAfter: 30 * time.Minute,
	}}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	st.CreateJob(ctx, &job.Job{
		State: job.StatePending, Category: "sonarr", NZBName: "Rel", NZBContent: []byte("x"),
	})

	if err := w.submitOnce(ctx); err != nil {
		t.Fatalf("submitOnce: %v", err)
	}
	d := time.Until(w.submitBackoffUntil)
	if d < 25*time.Minute || d > 31*time.Minute {
		t.Errorf("cooldown should follow the Retry-After hint (~30m), got %s", d)
	}
}

func TestPollerProgressUpdate(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateQueued, Category: "sonarr", NZBName: "Rel"})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 100
	st.UpdateJob(ctx, j)

	fake.list = []torbox.UsenetDownload{{
		ID: 100, Name: "Rel", Size: 2000, Progress: 0.5,
		DownloadState: "downloading", ETA: 120,
	}}
	if err := w.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateDownloading || got.ProgressPct != 50 {
		t.Errorf("got state=%s pct=%d", got.State, got.ProgressPct)
	}
	if got.ETASeconds != 120 {
		t.Errorf("eta not propagated: got %d want 120", got.ETASeconds)
	}
}

func TestPollerMarksFailed(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateQueued, Category: "sonarr", NZBName: "Rel"})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 300
	st.UpdateJob(ctx, j)
	fake.list = []torbox.UsenetDownload{{
		ID:            300,
		DownloadState: "failed (Repair failed, not enough repair blocks (73 short))",
	}}
	w.pollOnce(ctx)
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateFailed {
		t.Errorf("state: got %s want failed", got.State)
	}
	if got.FailMessage == "" {
		t.Error("fail_message should carry the TorBox reason for Sonarr")
	}
}

func TestPollerSkipsJobMissingFromList(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateQueued, Category: "sonarr", NZBName: "Ghost"})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 999
	st.UpdateJob(ctx, j)
	fake.list = nil // job 999 is not in the TorBox list
	if err := w.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateQueued {
		t.Errorf("missing job should stay queued, got %s", got.State)
	}
}

func TestPollerFailsJobMissingFromListAfterThreshold(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	old := missingPollThreshold
	missingPollThreshold = 3
	defer func() { missingPollThreshold = old }()

	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateQueued, Category: "sonarr", NZBName: "Vanished"})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 777
	st.UpdateJob(ctx, j)
	fake.list = nil // job 777 is never in the TorBox list

	for i := 1; i < missingPollThreshold; i++ {
		if err := w.pollOnce(ctx); err != nil {
			t.Fatalf("pollOnce %d: %v", i, err)
		}
		if got, _ := st.GetJob(ctx, id); got.State != job.StateQueued {
			t.Fatalf("poll %d: job failed too early (state %s)", i, got.State)
		}
	}
	if err := w.pollOnce(ctx); err != nil { // threshold reached
		t.Fatalf("pollOnce final: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateFailed {
		t.Errorf("state: got %s want failed after %d misses", got.State, missingPollThreshold)
	}
	if got.FailMessage == "" {
		t.Error("fail_message should explain the disappearance to Sonarr")
	}
}

func TestPollerMissCounterResetsOnReappear(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	old := missingPollThreshold
	missingPollThreshold = 3
	defer func() { missingPollThreshold = old }()

	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateQueued, Category: "sonarr", NZBName: "Flaky"})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 888
	st.UpdateJob(ctx, j)

	fake.list = nil
	w.pollOnce(ctx) // miss 1
	w.pollOnce(ctx) // miss 2

	// Job reappears before the threshold; counter must reset.
	fake.list = []torbox.UsenetDownload{{ID: 888, DownloadState: "downloading", Progress: 0.3, Size: 100}}
	w.pollOnce(ctx)

	fake.list = nil
	w.pollOnce(ctx) // miss 1 again, not 3
	w.pollOnce(ctx) // miss 2
	if got, _ := st.GetJob(ctx, id); got.State == job.StateFailed {
		t.Error("counter should have reset when the job reappeared in the list")
	}
}

func TestPollerCompletedWaitsForMissingPath(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	oldInterval, oldTimeout := pathRetryInterval, pathRetryTimeout
	pathRetryInterval, pathRetryTimeout = time.Millisecond, 10*time.Millisecond
	defer func() { pathRetryInterval, pathRetryTimeout = oldInterval, oldTimeout }()

	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateDownloading, Category: "sonarr", NZBName: "NoDir"})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 400
	st.UpdateJob(ctx, j)
	fake.list = []torbox.UsenetDownload{{
		ID: 400, Name: "NoDir.Missing", Size: 100, Progress: 1,
		DownloadFinished: true, DownloadPresent: true,
	}}
	if err := w.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State == job.StateCompleted {
		t.Error("job should not complete while webdav path is missing")
	}
}

func TestRunStartsAndStopsOnContextCancel(t *testing.T) {
	w, _, _ := testWorkers(t, &fakeTorBox{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancel")
	}
}

func TestDeleterRemovesFromTorBoxAndDropsRow(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateDeleted, Category: "sonarr", NZBName: "Del"})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 555
	st.UpdateJob(ctx, j)

	if err := w.deleteOnce(ctx); err != nil {
		t.Fatalf("deleteOnce: %v", err)
	}
	if len(fake.controls) != 1 || fake.controls[0] != "delete" {
		t.Errorf("expected one torbox delete, got %v", fake.controls)
	}
	if _, err := st.GetJob(ctx, id); err == nil {
		t.Error("job row should be removed after a successful delete")
	}
}

func TestDeleterRetriesThenGivesUp(t *testing.T) {
	fake := &fakeTorBox{controlErr: &torbox.APIError{Status: 500, Detail: "try again later"}}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateDeleted, Category: "sonarr", NZBName: "Del"})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 556
	st.UpdateJob(ctx, j)

	old := deleteGiveUpAttempts
	deleteGiveUpAttempts = 2
	defer func() { deleteGiveUpAttempts = old }()

	// First failure: the row is kept for the next cycle.
	if err := w.deleteOnce(ctx); err != nil {
		t.Fatalf("deleteOnce: %v", err)
	}
	if _, err := st.GetJob(ctx, id); err != nil {
		t.Fatal("job should be kept for retry while the TorBox delete fails")
	}

	// Second failure reaches deleteGiveUpAttempts: the row is dropped.
	if err := w.deleteOnce(ctx); err != nil {
		t.Fatalf("deleteOnce (give up): %v", err)
	}
	if _, err := st.GetJob(ctx, id); err == nil {
		t.Error("job row should be dropped once the give-up threshold is reached")
	}
}

func TestPollerCompletesAndBuildsFarm(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, cfg := testWorkers(t, fake)
	ctx := context.Background()

	relName := "The.Rookie.S08E01.GERMAN"
	srcDir := filepath.Join(cfg.UsenetPath(), relName)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "ep.mkv"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}

	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateDownloading, Category: "sonarr-stream", NZBName: relName})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 10
	st.UpdateJob(ctx, j)
	fake.list = []torbox.UsenetDownload{{
		ID: 10, Name: relName, Size: 1, Progress: 1,
		DownloadFinished: true, DownloadPresent: true,
	}}

	if err := w.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateCompleted {
		t.Fatalf("state: got %s want completed", got.State)
	}
	wantStorage := filepath.Join(cfg.SymlinkRoot, "sonarr-stream", relName)
	if got.StoragePath != wantStorage {
		t.Errorf("storage_path: got %q want %q", got.StoragePath, wantStorage)
	}
	if _, err := os.Lstat(filepath.Join(wantStorage, "ep.mkv")); err != nil {
		t.Errorf("symlink not created: %v", err)
	}
	if got.CompletedAt == nil {
		t.Error("completed_at not set")
	}
}

func TestPollerWaitsForEmptyWebDAVRelease(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, cfg := testWorkers(t, fake)
	ctx := context.Background()

	// TorBox reports the download finished and the WebDAV release folder
	// exists, but its files have not surfaced yet.
	relName := "Bleach.S01E15.2004.1080p"
	srcDir := filepath.Join(cfg.UsenetPath(), relName)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StateDownloading, Category: "sonarr-stream", NZBName: relName,
	})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 11
	st.UpdateJob(ctx, j)
	fake.list = []torbox.UsenetDownload{{
		ID: 11, Name: relName, Size: 1, Progress: 1,
		DownloadFinished: true, DownloadPresent: true,
	}}

	if err := w.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State == job.StateCompleted {
		t.Fatal("a release with no files yet must not be marked completed")
	}
	// The premature empty symlink dir must not be left for the reaper to find.
	prematureDir := filepath.Join(cfg.SymlinkRoot, "sonarr-stream", relName)
	if _, err := os.Stat(prematureDir); err == nil {
		t.Error("an empty symlink dir must not be published")
	}

	// Once the files surface, the next poll completes the job normally.
	if err := os.WriteFile(filepath.Join(srcDir, "ep.mkv"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce after files appeared: %v", err)
	}
	got, _ = st.GetJob(ctx, id)
	if got.State != job.StateCompleted {
		t.Fatalf("state once files exist: got %s want completed", got.State)
	}
	if _, err := os.Lstat(filepath.Join(prematureDir, "ep.mkv")); err != nil {
		t.Errorf("symlink not created after files surfaced: %v", err)
	}
}

func TestDeleterRemovesSymlinkDir(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, cfg := testWorkers(t, fake)
	ctx := context.Background()

	farm := filepath.Join(cfg.SymlinkRoot, "sonarr-stream", "Rel")
	if err := os.MkdirAll(farm, 0o755); err != nil {
		t.Fatal(err)
	}
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateDeleted, Category: "sonarr-stream", NZBName: "Rel"})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 11
	j.StoragePath = farm
	st.UpdateJob(ctx, j)

	if err := w.deleteOnce(ctx); err != nil {
		t.Fatalf("deleteOnce: %v", err)
	}
	if _, err := os.Stat(farm); err == nil {
		t.Error("symlink dir should be removed by the deleter")
	}
	if _, err := st.GetJob(ctx, id); err == nil {
		t.Error("job row should be removed")
	}
}

func TestDetectImportsAdvancesEmptiedReleases(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, cfg := testWorkers(t, fake)
	cfg.Categories = []string{"sonarr"}
	ctx := context.Background()
	catDir := filepath.Join(cfg.SymlinkRoot, "sonarr")

	// completedJob creates a completed job with a storage_path (which is
	// persisted via UpdateJob, not CreateJob).
	completedJob := func(name, storagePath string) int64 {
		t.Helper()
		id, _ := st.CreateJob(ctx, &job.Job{
			State: job.StateCompleted, Category: "sonarr", NZBName: name,
		})
		j, _ := st.GetJob(ctx, id)
		j.StoragePath = storagePath
		if err := st.UpdateJob(ctx, j); err != nil {
			t.Fatalf("UpdateJob: %v", err)
		}
		return id
	}

	// 1. Sonarr moved every symlink out — empty dir — counts as imported.
	emptyDir := filepath.Join(catDir, "Imported.Rel")
	os.MkdirAll(emptyDir, 0o755)
	emptyID := completedJob("Imported.Rel", emptyDir)

	// 2. Files still present — Sonarr has not imported — stays completed.
	fullDir := filepath.Join(catDir, "Waiting.Rel")
	os.MkdirAll(fullDir, 0o755)
	src := filepath.Join(t.TempDir(), "ep.mkv")
	os.WriteFile(src, []byte("v"), 0o644)
	os.Symlink(src, filepath.Join(fullDir, "ep.mkv"))
	fullID := completedJob("Waiting.Rel", fullDir)

	// 3. Release dir already swept away — counts as imported.
	goneID := completedJob("Gone.Rel", filepath.Join(catDir, "Gone.Rel"))

	if err := w.reapOnce(ctx); err != nil {
		t.Fatalf("reapOnce: %v", err)
	}
	if got, _ := st.GetJob(ctx, emptyID); got.State != job.StateImported {
		t.Errorf("an emptied release must become imported, got %s", got.State)
	}
	if got, _ := st.GetJob(ctx, fullID); got.State != job.StateCompleted {
		t.Errorf("a release whose files are still present must stay completed, got %s", got.State)
	}
	if got, _ := st.GetJob(ctx, goneID); got.State != job.StateImported {
		t.Errorf("a swept-away release must become imported, got %s", got.State)
	}
}

func TestReaperSweepsSymlinkFarm(t *testing.T) {
	fake := &fakeTorBox{}
	w, _, cfg := testWorkers(t, fake)
	cfg.Categories = []string{"sonarr-stream"}
	ctx := context.Background()
	catDir := filepath.Join(cfg.SymlinkRoot, "sonarr-stream")

	emptyDir := filepath.Join(catDir, "Imported.Release")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	orphanDir := filepath.Join(catDir, "Orphan.Release")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone.mkv"), filepath.Join(orphanDir, "gone.mkv")); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(t.TempDir(), "live.mkv")
	if err := os.WriteFile(srcFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	liveDir := filepath.Join(catDir, "Live.Release")
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(srcFile, filepath.Join(liveDir, "live.mkv")); err != nil {
		t.Fatal(err)
	}

	if err := w.reapOnce(ctx); err != nil {
		t.Fatalf("reapOnce: %v", err)
	}
	if _, err := os.Stat(emptyDir); err == nil {
		t.Error("empty dir should be swept")
	}
	if _, err := os.Stat(orphanDir); err == nil {
		t.Error("orphan (all-broken) dir should be swept")
	}
	if _, err := os.Stat(liveDir); err != nil {
		t.Error("live dir should be kept")
	}
}

func TestReaperRemovesOldImported(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateImported, Category: "sonarr", NZBName: "Old"})
	if _, err := st.Exec(ctx,
		`UPDATE jobs SET updated_at = ? WHERE id = ?`,
		time.Now().Add(-48*time.Hour), id); err != nil {
		t.Fatal(err)
	}
	if err := w.reapOnce(ctx); err != nil {
		t.Fatalf("reapOnce: %v", err)
	}
	if _, err := st.GetJob(ctx, id); err == nil {
		t.Error("expected old imported job to be reaped")
	}
}

// webdavRefreshServer is an httptest server standing in for TorBox's
// /refresh endpoint; it counts authenticated hits.
func webdavRefreshServer(t *testing.T, hits *atomic.Int32, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "wuser" || p != "wpass" {
			t.Errorf("bad basic auth: %q/%q", u, p)
		}
		hits.Add(1)
		w.WriteHeader(status)
	}))
}

// shortPathRetry shrinks the resolveStoragePath retry window for tests.
func shortPathRetry(t *testing.T) {
	t.Helper()
	oldI, oldT := pathRetryInterval, pathRetryTimeout
	pathRetryInterval, pathRetryTimeout = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { pathRetryInterval, pathRetryTimeout = oldI, oldT })
}

func TestPollerRefreshesWebDAVWhenAllFinished(t *testing.T) {
	var hits atomic.Int32
	srv := webdavRefreshServer(t, &hits, http.StatusOK)
	defer srv.Close()
	shortPathRetry(t)

	w, st, _ := testWorkers(t, &fakeTorBox{})
	w.cfg.TorBoxWebDAVUser = "wuser"
	w.cfg.TorBoxWebDAVPass = "wpass"
	w.cfg.TorBoxWebDAVRefreshURL = srv.URL
	w.cfg.WebDAVRefreshCooldown = time.Minute

	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateDownloading, Category: "sonarr", NZBName: "Done"})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 1
	st.UpdateJob(ctx, j)
	// TorBox reports finished, but the release folder is not on the mount yet.
	fake := w.tb.(*fakeTorBox)
	fake.list = []torbox.UsenetDownload{{
		ID: 1, Name: "Done.Missing", Size: 100, Progress: 1,
		DownloadFinished: true, DownloadPresent: true,
	}}

	if err := w.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 webdav refresh, got %d", hits.Load())
	}
	// A second poll within the cooldown must not refresh again.
	if err := w.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce 2: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("cooldown breached: %d refreshes", hits.Load())
	}
}

func TestPollerSkipsRefreshWhileDownloadOngoing(t *testing.T) {
	var hits atomic.Int32
	srv := webdavRefreshServer(t, &hits, http.StatusOK)
	defer srv.Close()
	shortPathRetry(t)

	w, st, _ := testWorkers(t, &fakeTorBox{})
	w.cfg.TorBoxWebDAVUser = "wuser"
	w.cfg.TorBoxWebDAVPass = "wpass"
	w.cfg.TorBoxWebDAVRefreshURL = srv.URL
	w.cfg.WebDAVRefreshCooldown = time.Minute

	ctx := context.Background()
	a, _ := st.CreateJob(ctx, &job.Job{State: job.StateDownloading, Category: "sonarr", NZBName: "A"})
	ja, _ := st.GetJob(ctx, a)
	ja.TorBoxID = 1
	st.UpdateJob(ctx, ja)
	b, _ := st.CreateJob(ctx, &job.Job{State: job.StateDownloading, Category: "sonarr", NZBName: "B"})
	jb, _ := st.GetJob(ctx, b)
	jb.TorBoxID = 2
	st.UpdateJob(ctx, jb)

	fake := w.tb.(*fakeTorBox)
	fake.list = []torbox.UsenetDownload{
		{ID: 1, Name: "A.Missing", Size: 100, Progress: 1, DownloadFinished: true, DownloadPresent: true},
		{ID: 2, Name: "B", Size: 100, Progress: 0.5, DownloadState: "downloading"},
	}
	if err := w.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("must not refresh while a download is still running, got %d", hits.Load())
	}
}

func TestSubmitterKeepsNZBContent(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StatePending, Category: "sonarr", NZBName: "Rel",
		NZBContent: []byte("<nzb/>"),
	})
	if err := w.submitOnce(ctx); err != nil {
		t.Fatalf("submitOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if len(got.NZBContent) == 0 {
		t.Error("nzb_content must be kept after submission (heal seed)")
	}
}

func TestPollerWebDAVRefreshBacksOffOn429(t *testing.T) {
	var hits atomic.Int32
	srv := webdavRefreshServer(t, &hits, http.StatusTooManyRequests)
	defer srv.Close()
	shortPathRetry(t)

	w, st, _ := testWorkers(t, &fakeTorBox{})
	w.cfg.TorBoxWebDAVUser = "wuser"
	w.cfg.TorBoxWebDAVPass = "wpass"
	w.cfg.TorBoxWebDAVRefreshURL = srv.URL
	w.cfg.WebDAVRefreshCooldown = time.Nanosecond // cooldown alone would not block

	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateDownloading, Category: "sonarr", NZBName: "Done"})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 1
	st.UpdateJob(ctx, j)
	fake := w.tb.(*fakeTorBox)
	fake.list = []torbox.UsenetDownload{{
		ID: 1, Name: "Done.Missing", Size: 100, Progress: 1,
		DownloadFinished: true, DownloadPresent: true,
	}}
	w.pollOnce(ctx) // first hit -> 429 -> backoff
	w.pollOnce(ctx) // blocked by the backoff despite the tiny cooldown
	if hits.Load() != 1 {
		t.Errorf("429 should trigger backoff; got %d refresh attempts", hits.Load())
	}
}

func TestHealRunInfoZeroBeforeFirstRun(t *testing.T) {
	w, _, _ := testWorkers(t, &fakeTorBox{})
	last, next := w.HealRunInfo()
	if !last.IsZero() || !next.IsZero() {
		t.Errorf("expected zero times before the first heal run, got %v / %v", last, next)
	}
}

func TestHealerDetectsBrokenSymlinks(t *testing.T) {
	w, st, _ := testWorkers(t, &fakeTorBox{})
	ctx := context.Background()
	jobID, _ := st.CreateJob(ctx, &job.Job{State: job.StateImported, Category: "c", NZBName: "n"})

	// A live symlink and a broken one.
	src := t.TempDir()
	good := filepath.Join(src, "good.mkv")
	os.WriteFile(good, []byte("x"), 0o644)
	lib := t.TempDir()
	liveLink := filepath.Join(lib, "live.mkv")
	os.Symlink(good, liveLink)
	brokenLink := filepath.Join(lib, "broken.mkv")
	os.Symlink(filepath.Join(src, "gone.mkv"), brokenLink)
	goneLink := filepath.Join(lib, "gone-entirely.mkv")
	os.Symlink(good, goneLink)

	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{JobID: jobID, SymlinkPath: liveLink, TargetPath: good})
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{JobID: jobID, SymlinkPath: brokenLink, TargetPath: filepath.Join(src, "gone.mkv")})
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{JobID: jobID, SymlinkPath: goneLink, TargetPath: good})
	os.Remove(goneLink) // the symlink itself disappears

	if err := w.detectBrokenSymlinks(ctx); err != nil {
		t.Fatalf("detectBrokenSymlinks: %v", err)
	}
	syms, _ := st.ListImportedSymlinks(ctx)
	got := map[string]bool{}
	for _, s := range syms {
		got[filepath.Base(s.SymlinkPath)] = s.IsBroken
	}
	if len(syms) != 2 {
		t.Fatalf("the vanished symlink row should be deleted; got %d rows", len(syms))
	}
	if got["live.mkv"] {
		t.Error("live symlink wrongly marked broken")
	}
	if !got["broken.mkv"] {
		t.Error("broken symlink not marked broken")
	}
}

func TestHealerDiscoversLibrarySymlinks(t *testing.T) {
	w, st, cfg := testWorkers(t, &fakeTorBox{})
	libRoot := t.TempDir()
	cfg.HealLibraryRoots = []string{libRoot}
	ctx := context.Background()

	// A completed job whose release folder is "Rel.A".
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateImported, Category: "sonarr", NZBName: "Rel.A"})
	j, _ := st.GetJob(ctx, id)
	j.StoragePath = filepath.Join(cfg.SymlinkRoot, "sonarr", "Rel.A")
	st.UpdateJob(ctx, j)

	// A library symlink pointing into the WebDAV mount for that release.
	target := filepath.Join(cfg.WebDAVMountRoot, "Rel.A", "ep.mkv")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(target, []byte("v"), 0o644)
	link := filepath.Join(libRoot, "Show", "ep.mkv")
	os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// A symlink pointing somewhere else entirely — must be ignored.
	other := filepath.Join(t.TempDir(), "elsewhere.mkv")
	os.WriteFile(other, []byte("x"), 0o644)
	os.Symlink(other, filepath.Join(libRoot, "Show", "other.mkv"))

	if err := w.discoverSymlinks(ctx); err != nil {
		t.Fatalf("discoverSymlinks: %v", err)
	}
	syms, _ := st.ListImportedSymlinks(ctx)
	if len(syms) != 1 {
		t.Fatalf("expected 1 tracked symlink, got %d", len(syms))
	}
	if syms[0].SymlinkPath != link || syms[0].JobID != id {
		t.Errorf("bad tracked symlink: %+v", syms[0])
	}
}

func TestHealerTriggersResubmission(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, cfg := testWorkers(t, fake)
	cfg.HealMaxAttempts = 3
	cfg.HealBackoffInitial = time.Minute
	ctx := context.Background()

	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StateImported, Category: "sonarr", NZBName: "Rel",
		NZBContent: []byte("<nzb/>"),
	})
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: id, SymlinkPath: "/lib/ep.mkv", TargetPath: "/mnt/torbox/Rel/ep.mkv",
	})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())

	if err := w.triggerHeals(ctx); err != nil {
		t.Fatalf("triggerHeals: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateHealing {
		t.Errorf("state: got %s want healing", got.State)
	}
	if got.TorBoxID == 0 {
		t.Error("a new torbox id should be recorded")
	}
	if len(fake.created) != 1 {
		t.Errorf("expected 1 resubmission, got %d", len(fake.created))
	}
}

func TestHealerSkipsExhaustedJobs(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, cfg := testWorkers(t, fake)
	cfg.HealMaxAttempts = 2
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StateHealFailed, Category: "c", NZBName: "n", NZBContent: []byte("x"),
	})
	j, _ := st.GetJob(ctx, id)
	j.HealCount = 2 // already at the limit
	st.UpdateJob(ctx, j)
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{JobID: id, SymlinkPath: "/lib/x.mkv", TargetPath: "/mnt/torbox/N/x.mkv"})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())

	if err := w.triggerHeals(ctx); err != nil {
		t.Fatalf("triggerHeals: %v", err)
	}
	if len(fake.created) != 0 {
		t.Error("a job at HealMaxAttempts must not be resubmitted")
	}
}

func TestHealReconcileFinishesHeal(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, cfg := testWorkers(t, fake)
	shortPathRetry(t)
	ctx := context.Background()

	// New release folder on the WebDAV mount (under the usenet subpath).
	newRel := "Rel.Healed"
	newDir := filepath.Join(cfg.UsenetPath(), newRel)
	os.MkdirAll(newDir, 0o755)
	os.WriteFile(filepath.Join(newDir, "ep.mkv"), []byte("v"), 0o644)

	// A job in `healing` with a broken library symlink.
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealing, Category: "sonarr", NZBName: newRel})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 700
	j.StoragePath = filepath.Join(cfg.SymlinkRoot, "sonarr", "Rel.Old")
	st.UpdateJob(ctx, j)

	lib := t.TempDir()
	link := filepath.Join(lib, "ep.mkv")
	os.Symlink(filepath.Join(cfg.UsenetPath(), "Rel.Old", "ep.mkv"), link)
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: id, SymlinkPath: link,
		TargetPath: filepath.Join(cfg.UsenetPath(), "Rel.Old", "ep.mkv"),
	})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())

	fake.list = []torbox.UsenetDownload{{
		ID: 700, Name: newRel, Progress: 1,
		DownloadFinished: true, DownloadPresent: true,
	}}
	if err := w.healReconcileOnce(ctx); err != nil {
		t.Fatalf("healReconcileOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateImported {
		t.Fatalf("state: got %s want imported", got.State)
	}
	if got.HealCount != 0 {
		t.Errorf("heal_count must reset to 0 on a successful heal: got %d", got.HealCount)
	}
	target, _ := os.Readlink(link)
	want := filepath.Join(newDir, "ep.mkv")
	if target != want {
		t.Errorf("symlink not repointed: got %q want %q", target, want)
	}
}

func TestHealReconcileNoMatchMarksFailed(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, cfg := testWorkers(t, fake)
	shortPathRetry(t)
	ctx := context.Background()

	// The new release folder exists but holds no file matching the broken
	// symlink, so findBestMatch cannot match it.
	newRel := "Rel.NoMatch"
	newDir := filepath.Join(cfg.UsenetPath(), newRel)
	os.MkdirAll(newDir, 0o755)
	os.WriteFile(filepath.Join(newDir, "unrelated.txt"), []byte("x"), 0o644)

	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealing, Category: "sonarr", NZBName: newRel})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 720
	j.StoragePath = filepath.Join(cfg.SymlinkRoot, "sonarr", "Rel.Old")
	st.UpdateJob(ctx, j)

	lib := t.TempDir()
	link := filepath.Join(lib, "ep.mkv")
	os.Symlink(filepath.Join(cfg.UsenetPath(), "Rel.Old", "ep.mkv"), link)
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: id, SymlinkPath: link,
		TargetPath: filepath.Join(cfg.UsenetPath(), "Rel.Old", "ep.mkv"),
	})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())

	fake.list = []torbox.UsenetDownload{{
		ID: 720, Name: newRel, Progress: 1,
		DownloadFinished: true, DownloadPresent: true,
	}}
	if err := w.healReconcileOnce(ctx); err != nil {
		t.Fatalf("healReconcileOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateHealFailed {
		t.Fatalf("a heal that repointed nothing must be heal_failed, got %s", got.State)
	}
	if got.HealCount != 1 || got.LastHealError == "" {
		t.Errorf("the failed heal must be recorded: count=%d err=%q", got.HealCount, got.LastHealError)
	}
}

func TestHealReconcileMarksFailedDownload(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealing, Category: "c", NZBName: "n"})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 701
	st.UpdateJob(ctx, j)
	fake.list = []torbox.UsenetDownload{{ID: 701, DownloadState: "failed (dead)"}}
	if err := w.healReconcileOnce(ctx); err != nil {
		t.Fatalf("healReconcileOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateHealFailed {
		t.Errorf("state: got %s want heal_failed", got.State)
	}
}

func TestHealerDryRunDoesNotResubmit(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, cfg := testWorkers(t, fake)
	cfg.HealMaxAttempts = 3
	cfg.HealDryRun = true
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StateImported, Category: "c", NZBName: "n", NZBContent: []byte("x"),
	})
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: id, SymlinkPath: "/lib/x.mkv", TargetPath: "/mnt/torbox/N/x.mkv",
	})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())

	if err := w.triggerHeals(ctx); err != nil {
		t.Fatalf("triggerHeals: %v", err)
	}
	if len(fake.created) != 0 {
		t.Error("dry-run must not resubmit anything to TorBox")
	}
	if got, _ := st.GetJob(ctx, id); got.State != job.StateImported {
		t.Errorf("dry-run must leave the job state unchanged, got %s", got.State)
	}
}

func TestHealWebhookWants(t *testing.T) {
	events := []string{"failed", "healed"}
	if !healWebhookWants(events, "failed") {
		t.Error("failed should be wanted")
	}
	if healWebhookWants(events, "detected") {
		t.Error("detected should not be wanted")
	}
}

func TestEmitHealEventPostsPayload(t *testing.T) {
	received := make(chan webhookPayload, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type: %q", r.Header.Get("Content-Type"))
		}
		var p webhookPayload
		json.NewDecoder(r.Body).Decode(&p)
		received <- p
	}))
	defer srv.Close()

	w, _, cfg := testWorkers(t, &fakeTorBox{})
	cfg.HealWebhookURL = srv.URL
	cfg.HealWebhookEvents = []string{"healed"}

	j := &job.Job{ID: 7, NZBName: "Rel", Category: "sonarr", HealCount: 1}
	w.emitHealEvent("healed", j, healEventExtra{SymlinksHealed: 2, NewTorBoxID: 99})

	select {
	case p := <-received:
		if p.Event != "healed" || p.Job.ID != 7 || p.SymlinksHealed != 2 || p.NewTorBoxID != 99 {
			t.Errorf("bad payload: %+v", p)
		}
		if p.Timestamp == "" {
			t.Error("timestamp must be set")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook was not delivered")
	}
}

func TestEmitHealEventSkipsUnwantedAndUnconfigured(t *testing.T) {
	hits := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
	}))
	defer srv.Close()

	w, _, cfg := testWorkers(t, &fakeTorBox{})
	j := &job.Job{ID: 1, NZBName: "n"}

	// Unconfigured URL: no POST.
	w.emitHealEvent("failed", j, healEventExtra{})
	// Configured, but the event is not in the wanted set: no POST.
	cfg.HealWebhookURL = srv.URL
	cfg.HealWebhookEvents = []string{"healed"}
	w.emitHealEvent("failed", j, healEventExtra{})

	select {
	case <-hits:
		t.Fatal("webhook fired when it should not have")
	case <-time.After(300 * time.Millisecond):
		// expected — nothing delivered
	}
}

func TestHealerFiresFailedEvent(t *testing.T) {
	received := make(chan webhookPayload, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p webhookPayload
		json.NewDecoder(r.Body).Decode(&p)
		received <- p
	}))
	defer srv.Close()

	fake := &fakeTorBox{createErr: &torbox.APIError{Status: 500, Detail: "down"}}
	w, st, cfg := testWorkers(t, fake)
	cfg.HealMaxAttempts = 3
	cfg.HealWebhookURL = srv.URL
	cfg.HealWebhookEvents = []string{"detected", "failed"}
	ctx := context.Background()

	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StateImported, Category: "c", NZBName: "n", NZBContent: []byte("x"),
	})
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: id, SymlinkPath: "/lib/x.mkv", TargetPath: "/mnt/torbox/N/x.mkv",
	})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())

	if err := w.triggerHeals(ctx); err != nil {
		t.Fatalf("triggerHeals: %v", err)
	}
	// Expect a "detected" and a "failed" event (order not guaranteed).
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case p := <-received:
			seen[p.Event] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("missing webhook events; got %v", seen)
		}
	}
	if !seen["detected"] || !seen["failed"] {
		t.Errorf("expected detected+failed events, got %v", seen)
	}
}
