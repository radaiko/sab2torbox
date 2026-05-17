package torbox

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCreateUsenetDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/usenet/createusenetdownload" {
			t.Errorf("path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("auth header: %q", got)
		}
		ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if ct != "multipart/form-data" {
			t.Errorf("content-type: %q", ct)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		b, _ := io.ReadAll(f)
		if string(b) != "<nzb/>" {
			t.Errorf("nzb content: %q", b)
		}
		w.Write([]byte(`{"success":true,"data":{"usenetdownload_id":"55","hash":"h","auth_id":"a"}}`))
	}))
	defer srv.Close()

	c := NewWithBaseURL("tok", srv.URL+"/v1/api")
	res, err := c.CreateUsenetDownload(context.Background(), CreateRequest{
		NZBContent: []byte("<nzb/>"), NZBName: "rel.nzb",
	})
	if err != nil {
		t.Fatalf("CreateUsenetDownload: %v", err)
	}
	if int64(res.UsenetDownloadID) != 55 || res.Hash != "h" {
		t.Errorf("bad result: %+v", res)
	}
}

func TestListUsenet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"data":[{"id":1,"name":"A"},{"id":2,"name":"B"}]}`))
	}))
	defer srv.Close()
	c := NewWithBaseURL("tok", srv.URL+"/v1/api")
	list, err := c.ListUsenet(context.Background())
	if err != nil || len(list) != 2 {
		t.Fatalf("ListUsenet: len=%d err=%v", len(list), err)
	}
}

func TestControlUsenetDeleteBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"operation":"delete"`) ||
			!strings.Contains(string(b), `"usenet_id":7`) {
			t.Errorf("control body: %s", b)
		}
		w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()
	c := NewWithBaseURL("tok", srv.URL+"/v1/api")
	if err := c.ControlUsenet(context.Background(), 7, "delete"); err != nil {
		t.Fatalf("ControlUsenet: %v", err)
	}
}

func TestPingAndHTTPError(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer ok.Close()
	if err := NewWithBaseURL("tok", ok.URL+"/v1/api").Ping(context.Background()); err != nil {
		t.Errorf("Ping ok: %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"success":false,"detail":"boom"}`))
	}))
	defer bad.Close()
	err := NewWithBaseURL("tok", bad.URL+"/v1/api").Ping(context.Background())
	if err == nil {
		t.Fatal("expected error on HTTP 500")
	}
	if !Retryable(err) {
		t.Error("HTTP 500 error should be retryable")
	}
}

func TestRetryableAndDownloadedBytes(t *testing.T) {
	if Retryable(&APIError{Status: 400}) {
		t.Error("400 should not be retryable")
	}
	if !Retryable(&APIError{Status: 429}) {
		t.Error("429 should be retryable")
	}
	if !Retryable(io.EOF) {
		t.Error("transport errors should be retryable")
	}
	if got := (UsenetDownload{Size: 1000, Progress: 0.5}).DownloadedBytes(); got != 500 {
		t.Errorf("DownloadedBytes 0.5: got %d", got)
	}
	if got := (UsenetDownload{Size: 1000, Progress: 50}).DownloadedBytes(); got != 500 {
		t.Errorf("DownloadedBytes 50: got %d", got)
	}
	if got := (UsenetDownload{Size: 0, Progress: 1}).DownloadedBytes(); got != 0 {
		t.Errorf("DownloadedBytes size 0: got %d", got)
	}
}

func TestRateLimitParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"success":false,"detail":"60 per 1 hour"}`))
	}))
	defer srv.Close()
	c := NewWithBaseURL("tok", srv.URL+"/v1/api")
	_, err := c.CreateUsenetDownload(context.Background(), CreateRequest{NZBContent: []byte("x"), NZBName: "n"})
	retryAfter, ok := RateLimit(err)
	if !ok {
		t.Fatalf("a 429 must be recognised as a rate-limit, got %v", err)
	}
	if retryAfter != 2*time.Minute {
		t.Errorf("Retry-After: 120 should parse to 2m, got %s", retryAfter)
	}
	if _, ok := RateLimit(&APIError{Status: 500}); ok {
		t.Error("a 500 is not a rate-limit")
	}
	if d := parseRetryAfter(""); d != 0 {
		t.Errorf("absent Retry-After should be 0, got %s", d)
	}
	if d := parseRetryAfter("garbage"); d != 0 {
		t.Errorf("unparseable Retry-After should be 0, got %s", d)
	}
}

func TestNonJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>not json</html>"))
	}))
	defer srv.Close()
	_, err := NewWithBaseURL("tok", srv.URL+"/v1/api").ListUsenet(context.Background())
	if err == nil {
		t.Fatal("expected error on non-JSON response")
	}
}

func TestCreateUsenetAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":false,"detail":"bad nzb"}`))
	}))
	defer srv.Close()
	c := NewWithBaseURL("tok", srv.URL+"/v1/api")
	_, err := c.CreateUsenetDownload(context.Background(), CreateRequest{NZBContent: []byte("x"), NZBName: "n"})
	if err == nil || !strings.Contains(err.Error(), "bad nzb") {
		t.Fatalf("expected API error, got %v", err)
	}
}
