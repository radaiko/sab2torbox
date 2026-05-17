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
	"testing"
	"time"

	"github.com/radaiko/sab2torbox/internal/config"
	"github.com/radaiko/sab2torbox/internal/job"
	"github.com/radaiko/sab2torbox/internal/store"
	"github.com/radaiko/sab2torbox/internal/torbox"
)

// mockTorBox is an httptest-backed TorBox server with controllable state.
type mockTorBox struct {
	mu       sync.Mutex
	finished bool
	name     string
}

func (m *mockTorBox) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/api/usenet/createusenetdownload",
		func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"success":true,"data":{"usenetdownload_id":901,"hash":"h"}}`))
		})
	mux.HandleFunc("/v1/api/usenet/mylist",
		func(w http.ResponseWriter, r *http.Request) {
			m.mu.Lock()
			finished := m.finished
			m.mu.Unlock()
			rec := map[string]any{
				"id": 901, "name": m.name, "size": 1000,
				"download_state": "downloading", "progress": 0.4,
			}
			if finished {
				rec["download_state"] = "completed"
				rec["download_finished"] = true
				rec["download_present"] = true
				rec["progress"] = 1
			}
			resp := map[string]any{"success": true, "data": []any{rec}}
			json.NewEncoder(w).Encode(resp)
		})
	mux.HandleFunc("/v1/api/usenet/controlusenetdownload",
		func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"success":true}`))
		})
	return mux
}

func TestEndToEndSubmitPollCompleteDelete(t *testing.T) {
	mock := &mockTorBox{name: "Integration.Release"}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	mountRoot := t.TempDir()
	cfg := &config.Config{
		WebDAVMountRoot: mountRoot, WebDAVUsenetSubpath: "usenet",
		SymlinkRoot:  t.TempDir(),
		PollInterval: 5 * time.Millisecond,
	}
	tb := torbox.NewWithBaseURL("tok", srv.URL+"/v1/api")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := New(st, tb, cfg, logger)
	ctx := context.Background()

	// 1. Submit.
	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StatePending, Category: "sonarr",
		NZBName: "Integration.Release", NZBContent: []byte("<nzb/>"),
	})
	if err := w.submitOnce(ctx); err != nil {
		t.Fatalf("submitOnce: %v", err)
	}
	if j, _ := st.GetJob(ctx, id); j.State != job.StateQueued || j.TorBoxID != 901 {
		t.Fatalf("after submit: %+v", j)
	}

	// 2. Poll while downloading.
	if err := w.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce downloading: %v", err)
	}
	if j, _ := st.GetJob(ctx, id); j.State != job.StateDownloading {
		t.Fatalf("expected downloading, got %s", j.State)
	}

	// 3. TorBox finishes; the WebDAV directory and its file appear.
	relDir := filepath.Join(cfg.UsenetPath(), "Integration.Release")
	if err := os.MkdirAll(relDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(relDir, "ep.mkv"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	mock.mu.Lock()
	mock.finished = true
	mock.mu.Unlock()

	if err := w.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce completed: %v", err)
	}
	j, _ := st.GetJob(ctx, id)
	if j.State != job.StateCompleted {
		t.Fatalf("expected completed, got %s", j.State)
	}
	wantPath := filepath.Join(cfg.SymlinkRoot, "sonarr", "Integration.Release")
	if j.StoragePath != wantPath {
		t.Fatalf("storage path: got %q want %q", j.StoragePath, wantPath)
	}

	// 4. Delete from TorBox.
	if err := tb.ControlUsenet(ctx, j.TorBoxID, "delete"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}
