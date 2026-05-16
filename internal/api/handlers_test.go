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
	"strings"
	"testing"

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
		WebDAVUsenetSubpath: "usenet", Categories: []string{"sonarr", "radarr"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewServer(st, cfg, logger, nil), st
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

func TestHistoryDeleteWithFilesCallsTorBox(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateCompleted, Category: "sonarr", NZBName: "Del"})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 500
	st.UpdateJob(ctx, j)

	fake := &fakeDeleter{}
	srv.deleter = fake

	rec := httptest.NewRecorder()
	u := "/api?mode=history&name=delete&value=" + url.QueryEscape(j.NzoID()) +
		"&del_files=1&apikey=secret"
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u, nil))

	if len(fake.deleted) != 1 || fake.deleted[0] != 500 {
		t.Errorf("expected torbox delete of id 500, got %v", fake.deleted)
	}
	if _, err := st.GetJob(ctx, id); err == nil {
		t.Error("job row should be removed after delete")
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
	if get() != get() {
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

// fakeDeleter records TorBox delete calls.
type fakeDeleter struct{ deleted []int64 }

func (f *fakeDeleter) ControlUsenet(_ context.Context, id int64, op string) error {
	if op == "delete" {
		f.deleted = append(f.deleted, id)
	}
	return nil
}

// fakeHealth is a canned Checker.
type fakeHealth struct{ ok bool }

func (f *fakeHealth) Check(context.Context) error {
	if f.ok {
		return nil
	}
	return io.EOF
}
