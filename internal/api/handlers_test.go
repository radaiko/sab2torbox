package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/radaiko/sab2torbox/internal/config"
	"github.com/radaiko/sab2torbox/internal/job"
	"github.com/radaiko/sab2torbox/internal/store"
)

// newAPITestStore opens a throwaway migrated store.
func newAPITestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := newAPITestStore(t)
	cfg := &config.Config{
		SABAPIKey: "secret", WebDAVMountRoot: t.TempDir(),
		WebDAVUsenetSubpath: "usenet", SymlinkRoot: t.TempDir(),
		Categories: []string{"sonarr", "radarr"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewServer(st, cfg, logger), st
}

func TestAuthRejectsBadKey(t *testing.T) {
	srv, _ := testServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api?mode=version&apikey=wrong", nil)
	srv.Router().ServeHTTP(rec, req)
	var resp ErrorResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Status {
		t.Errorf("expected auth failure, got %s", rec.Body.String())
	}
}

func TestVersion(t *testing.T) {
	srv, _ := testServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api?mode=version&apikey=secret", nil)
	srv.Router().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"version"`) {
		t.Errorf("version body: %s", rec.Body.String())
	}
}

func TestAddFileCreatesPendingJob(t *testing.T) {
	srv, st := testServer(t)
	var body strings.Builder
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("name", "Show.S01E01.nzb")
	fw.Write([]byte("<nzb/>"))
	mw.WriteField("cat", "sonarr")
	mw.WriteField("nzbname", "Show.S01E01")
	mw.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api?mode=addfile&apikey=secret", strings.NewReader(body.String()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	srv.Router().ServeHTTP(rec, req)

	var resp AddResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if !resp.Status || len(resp.NzoIDs) != 1 {
		t.Fatalf("bad add response: %+v", resp)
	}
	jobs, _ := st.JobsByState(context.Background(), job.StatePending)
	if len(jobs) != 1 || jobs[0].Category != "sonarr" || jobs[0].NZBName != "Show.S01E01" {
		t.Errorf("job not created correctly: %+v", jobs)
	}
}

func TestAddFileIdempotent(t *testing.T) {
	srv, st := testServer(t)
	post := func() string {
		var body strings.Builder
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("name", "Dup.nzb")
		fw.Write([]byte("<identical/>"))
		mw.WriteField("cat", "sonarr")
		mw.WriteField("nzbname", "Dup")
		mw.Close()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api?mode=addfile&apikey=secret", strings.NewReader(body.String()))
		req.Header.Set("Content-Type", mw.FormDataContentType())
		srv.Router().ServeHTTP(rec, req)
		var resp AddResponse
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if len(resp.NzoIDs) == 0 {
			t.Fatalf("no nzo_id in response: %s", rec.Body.String())
		}
		return resp.NzoIDs[0]
	}
	first, second := post(), post()
	if first != second {
		t.Errorf("idempotency broken: %s != %s", first, second)
	}
	jobs, _ := st.JobsByState(context.Background(), job.StatePending)
	if len(jobs) != 1 {
		t.Errorf("expected 1 job after duplicate submit, got %d", len(jobs))
	}
}

func TestQueueAndHistory(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	st.CreateJob(ctx, &job.Job{State: job.StateDownloading, Category: "sonarr", NZBName: "Active"})
	st.CreateJob(ctx, &job.Job{State: job.StateCompleted, Category: "sonarr", NZBName: "Done", StoragePath: "/p"})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?mode=queue&apikey=secret", nil))
	var q QueueResponse
	json.Unmarshal(rec.Body.Bytes(), &q)
	if len(q.Queue.Slots) != 1 || q.Queue.Slots[0].Filename != "Active" {
		t.Errorf("queue: %+v", q)
	}

	rec = httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?mode=history&apikey=secret", nil))
	var h HistoryResponse
	json.Unmarshal(rec.Body.Bytes(), &h)
	if len(h.History.Slots) != 1 || h.History.Slots[0].Name != "Done" {
		t.Errorf("history: %+v", h)
	}
}

func TestHistoryOmitsImportedJobs(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	cid, _ := st.CreateJob(ctx, &job.Job{
		State: job.StateCompleted, Category: "sonarr", NZBName: "Fresh", StoragePath: "/p",
	})
	st.CreateJob(ctx, &job.Job{
		State: job.StateImported, Category: "sonarr", NZBName: "AlreadyImported", StoragePath: "/gone",
	})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?mode=history&apikey=secret", nil))
	var h HistoryResponse
	json.Unmarshal(rec.Body.Bytes(), &h)
	if len(h.History.Slots) != 1 || h.History.Slots[0].Name != "Fresh" {
		t.Fatalf("history should list only the completed job, got %+v", h)
	}
	// A history read must not flip a completed job to imported: Sonarr polls
	// history continuously, long before it actually imports anything.
	if got, _ := st.GetJob(ctx, cid); got.State != job.StateCompleted {
		t.Errorf("completed job must stay completed after a history read, got %s", got.State)
	}
}

func TestHistoryDeleteWithFilesMarksJobDeleted(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateCompleted, Category: "sonarr", NZBName: "Del"})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 500
	st.UpdateJob(ctx, j)

	rec := httptest.NewRecorder()
	u := "/api?mode=history&name=delete&value=" + url.QueryEscape(j.NzoID()) +
		"&del_files=1&apikey=secret"
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u, nil))

	// del_files keeps the row for the deleter worker, in state "deleted".
	got, err := st.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("job row should be kept for the deleter worker: %v", err)
	}
	if got.State != job.StateDeleted {
		t.Errorf("state: got %s want deleted", got.State)
	}
}

func TestHealthz(t *testing.T) {
	srv, _ := testServer(t)
	srv.health = &fakeHealth{ok: true}
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthz: got %d", rec.Code)
	}
}

func TestGetConfigAndFullstatus(t *testing.T) {
	srv, _ := testServer(t)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?mode=get_config&apikey=secret", nil))
	var cfg ConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("get_config decode: %v", err)
	}
	if len(cfg.Config.Categories) != 3 || cfg.Config.Misc.CompleteDir == "" {
		t.Errorf("get_config: %+v", cfg.Config)
	}
	// "*" reports no dir; a named category maps to its own subdir under the
	// symlink root, which sab2torbox pre-creates for the health check.
	for _, c := range cfg.Config.Categories {
		want := c.Name
		if c.Name == "*" {
			want = ""
		}
		if c.Dir != want {
			t.Errorf("category %q reports dir %q, want %q", c.Name, c.Dir, want)
		}
	}

	rec = httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?mode=fullstatus&apikey=secret", nil))
	if !strings.Contains(rec.Body.String(), `"status"`) {
		t.Errorf("fullstatus: %s", rec.Body.String())
	}
}

func TestAddURLCreatesAndIsIdempotent(t *testing.T) {
	srv, st := testServer(t)
	u := "/api?mode=addurl&apikey=secret&cat=sonarr&name=" +
		url.QueryEscape("http://idx/x.nzb") + "&nzbname=Rel"
	get := func() string {
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u, nil))
		var resp AddResponse
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if !resp.Status || len(resp.NzoIDs) != 1 {
			t.Fatalf("addurl response: %s", rec.Body.String())
		}
		return resp.NzoIDs[0]
	}
	first, second := get(), get()
	if first != second {
		t.Error("addurl not idempotent")
	}
	jobs, _ := st.JobsByState(context.Background(), job.StatePending)
	if len(jobs) != 1 {
		t.Errorf("expected 1 job, got %d", len(jobs))
	}
}

func TestUnknownModeAndMissingFile(t *testing.T) {
	srv, _ := testServer(t)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?mode=bogus&apikey=secret", nil))
	if strings.Contains(rec.Body.String(), `"status":true`) {
		t.Errorf("unknown mode should fail: %s", rec.Body.String())
	}

	var body strings.Builder
	mw := multipart.NewWriter(&body)
	mw.WriteField("cat", "sonarr")
	mw.Close()
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api?mode=addfile&apikey=secret", strings.NewReader(body.String()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	srv.Router().ServeHTTP(rec, req)
	var resp ErrorResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Status {
		t.Errorf("addfile without file should fail: %s", rec.Body.String())
	}
}

func TestHealthzUnhealthyAndNil(t *testing.T) {
	srv, _ := testServer(t)
	srv.health = &fakeHealth{ok: false}
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unhealthy: got %d want 503", rec.Code)
	}

	srv.health = nil
	rec = httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("nil health: got %d want 200", rec.Code)
	}
}

func TestQueueDeleteWithoutFiles(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateQueued, Category: "sonarr", NZBName: "Q"})
	j, _ := st.GetJob(ctx, id)
	u := "/api?mode=queue&name=delete&value=" + url.QueryEscape(j.NzoID()) + "&apikey=secret"
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u, nil))
	if _, err := st.GetJob(ctx, id); err == nil {
		t.Error("job should be removed by queue delete")
	}
}

// fakeHealth is a canned Checker.
type fakeHealth struct{ ok bool }

func (f *fakeHealth) Check(context.Context) error {
	if f.ok {
		return nil
	}
	return io.EOF
}

// fakeHealReporter is a canned HealReporter.
type fakeHealReporter struct{ last, next time.Time }

func (f fakeHealReporter) HealRunInfo() (time.Time, time.Time) { return f.last, f.next }

func TestHealthSymlinks(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	jobID, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealing, Category: "c", NZBName: "n"})
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: jobID, SymlinkPath: "/lib/a.mkv", TargetPath: "/mnt/torbox/N/a.mkv",
	})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())
	srv.SetHealReporter(fakeHealReporter{last: time.Now()})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/symlinks", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var resp SymlinkHealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if resp.Tracked != 1 || resp.Broken != 1 || resp.Healing != 1 {
		t.Errorf("counts wrong: %+v", resp)
	}
}

func TestHealFailedEndpoint(t *testing.T) {
	srv, st := testServer(t)
	srv.cfg.HealMaxAttempts = 3
	ctx := context.Background()

	// An exhausted job (heal_count >= max) — should be listed.
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealFailed, Category: "sonarr", NZBName: "Dead"})
	j, _ := st.GetJob(ctx, id)
	j.HealCount = 3
	j.LastHealError = "nzb gone"
	st.UpdateJob(ctx, j)
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: id, SymlinkPath: "/lib/dead.mkv", TargetPath: "/mnt/torbox/Dead/dead.mkv",
	})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())

	// A heal_failed job still under the limit — should NOT be listed.
	id2, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealFailed, Category: "c", NZBName: "Retrying"})
	j2, _ := st.GetJob(ctx, id2)
	j2.HealCount = 1
	st.UpdateJob(ctx, j2)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/heal_failed", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var items []HealFailedItem
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(items) != 1 || items[0].JobID != id {
		t.Fatalf("expected only the exhausted job, got %+v", items)
	}
	if items[0].HealCount != 3 || items[0].LastHealError != "nzb gone" ||
		len(items[0].BrokenSymlinks) != 1 {
		t.Errorf("bad item: %+v", items[0])
	}
}

func TestHealRetryEndpoint(t *testing.T) {
	srv, st := testServer(t)
	srv.cfg.HealMaxAttempts = 3
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealFailed, Category: "c", NZBName: "n"})
	j, _ := st.GetJob(ctx, id)
	j.HealCount = 3
	j.LastHealError = "boom"
	st.UpdateJob(ctx, j)

	rec := httptest.NewRecorder()
	u := "/health/heal/" + itoaTest(id) + "/retry?apikey=secret"
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, u, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := st.GetJob(ctx, id)
	if got.HealCount != 0 || got.LastHealError != "" {
		t.Errorf("retry must reset heal_count and clear the error: %+v", got)
	}
}

func TestHealGiveUpEndpoint(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealFailed, Category: "c", NZBName: "n"})
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: id, SymlinkPath: "/lib/x.mkv", TargetPath: "/mnt/torbox/N/x.mkv",
	})

	rec := httptest.NewRecorder()
	u := "/health/heal/" + itoaTest(id) + "/give_up?apikey=secret"
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, u, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateManuallyResolved {
		t.Errorf("give_up must set manually_resolved, got %s", got.State)
	}
	syms, _ := st.ListImportedSymlinks(ctx)
	if len(syms) != 0 {
		t.Errorf("give_up must drop the job's tracked symlinks, got %d", len(syms))
	}
}

func TestHealRetryRejectsNonHealFailedJob(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateImported, Category: "c", NZBName: "n"})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/health/heal/"+itoaTest(id)+"/retry?apikey=secret", nil))
	var resp ErrorResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Status {
		t.Error("retry must reject a job that is not in heal_failed state")
	}
	if got, _ := st.GetJob(ctx, id); got.State != job.StateImported {
		t.Errorf("the job state must be unchanged, got %s", got.State)
	}
}

func TestHealGiveUpRejectsNonHealFailedJob(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealing, Category: "c", NZBName: "n"})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/health/heal/"+itoaTest(id)+"/give_up?apikey=secret", nil))
	var resp ErrorResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Status {
		t.Error("give_up must reject a job that is not in heal_failed state")
	}
	if got, _ := st.GetJob(ctx, id); got.State != job.StateHealing {
		t.Errorf("the job state must be unchanged, got %s", got.State)
	}
}

func TestHealEndpointsRequireAPIKey(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealFailed, Category: "c", NZBName: "n"})

	// No apikey: the state-mutating POST must be rejected and the job untouched.
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/health/heal/"+itoaTest(id)+"/retry", nil))
	var resp ErrorResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Status {
		t.Error("retry without the API key must be rejected")
	}
	if got, _ := st.GetJob(ctx, id); got.HealCount != 0 || got.State != job.StateHealFailed {
		t.Errorf("an unauthenticated retry must not mutate the job: %+v", got)
	}
}

// itoaTest renders an int64 for building test URLs.
func itoaTest(n int64) string { return strconv.FormatInt(n, 10) }
