package torbox

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
