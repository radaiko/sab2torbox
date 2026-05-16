package worker

import (
	"context"
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
	mu         sync.Mutex
	created    []torbox.CreateRequest
	createErr  error
	nextID     int64
	list       []torbox.UsenetDownload
	controls   []string
	controlErr error
}

func (f *fakeTorBox) CreateUsenetDownload(_ context.Context, r torbox.CreateRequest) (*torbox.CreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func TestPollerCompletesAndResolvesPath(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, cfg := testWorkers(t, fake)
	ctx := context.Background()
	relDir := filepath.Join(cfg.UsenetPath(), "Rel.Complete")
	if err := os.MkdirAll(relDir, 0o755); err != nil {
		t.Fatal(err)
	}
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateDownloading, Category: "sonarr", NZBName: "Rel.Complete"})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 200
	st.UpdateJob(ctx, j)

	fake.list = []torbox.UsenetDownload{{
		ID: 200, Name: "Rel.Complete", Size: 1000, Progress: 1,
		DownloadFinished: true, DownloadPresent: true, DownloadState: "completed",
	}}
	if err := w.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateCompleted {
		t.Fatalf("state: got %s want completed", got.State)
	}
	if got.StoragePath != relDir {
		t.Errorf("storage path: got %q want %q", got.StoragePath, relDir)
	}
	if got.CompletedAt == nil {
		t.Error("completed_at not set")
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

	// TorBox keeps failing: the row is kept for the next cycle.
	if err := w.deleteOnce(ctx); err != nil {
		t.Fatalf("deleteOnce: %v", err)
	}
	if _, err := st.GetJob(ctx, id); err != nil {
		t.Fatal("job should be kept for retry while the TorBox delete fails")
	}

	// Past the give-up window, the row is dropped despite the failure.
	old := deleteGiveUpAfter
	deleteGiveUpAfter = -time.Second
	defer func() { deleteGiveUpAfter = old }()
	if err := w.deleteOnce(ctx); err != nil {
		t.Fatalf("deleteOnce (give up): %v", err)
	}
	if _, err := st.GetJob(ctx, id); err == nil {
		t.Error("job row should be dropped once the give-up window has elapsed")
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
