package torbox

import (
	"encoding/json"
	"testing"
)

func TestFlexIntFromNumber(t *testing.T) {
	var v struct {
		ID FlexInt `json:"id"`
	}
	if err := json.Unmarshal([]byte(`{"id": 4242}`), &v); err != nil {
		t.Fatalf("unmarshal number: %v", err)
	}
	if int64(v.ID) != 4242 {
		t.Errorf("got %d, want 4242", v.ID)
	}
}

func TestFlexIntFromString(t *testing.T) {
	var v struct {
		ID FlexInt `json:"id"`
	}
	if err := json.Unmarshal([]byte(`{"id": "4242"}`), &v); err != nil {
		t.Fatalf("unmarshal string: %v", err)
	}
	if int64(v.ID) != 4242 {
		t.Errorf("got %d, want 4242", v.ID)
	}
}

func TestFlexIntNullAndEmpty(t *testing.T) {
	var v struct {
		ID FlexInt `json:"id"`
	}
	if err := json.Unmarshal([]byte(`{"id": null}`), &v); err != nil {
		t.Fatalf("unmarshal null: %v", err)
	}
	if int64(v.ID) != 0 {
		t.Errorf("null should yield 0, got %d", v.ID)
	}
	if err := json.Unmarshal([]byte(`{"id": ""}`), &v); err != nil {
		t.Fatalf("unmarshal empty string: %v", err)
	}
}

func TestDecodeUsenetListEnvelope(t *testing.T) {
	body := `{"success":true,"detail":"ok","data":[
		{"id":7,"name":"Rel.A","size":1024,"download_finished":true,
		 "download_present":true,"progress":1,"download_state":"completed",
		 "files":[{"id":"1","name":"a/Rel.A.mkv","short_name":"Rel.A.mkv","size":1024}]}]}`
	var env Envelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	var list []UsenetDownload
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatalf("data: %v", err)
	}
	if len(list) != 1 || int64(list[0].ID) != 7 || !list[0].DownloadFinished {
		t.Fatalf("bad decode: %+v", list)
	}
	if list[0].Files[0].ShortName != "Rel.A.mkv" {
		t.Errorf("file short_name: %q", list[0].Files[0].ShortName)
	}
}

func TestProgressPctAndFailed(t *testing.T) {
	if got := (UsenetDownload{Progress: 0.5}).ProgressPct(); got != 50 {
		t.Errorf("ProgressPct 0.5: got %d want 50", got)
	}
	if got := (UsenetDownload{Progress: 73}).ProgressPct(); got != 73 {
		t.Errorf("ProgressPct 73: got %d want 73", got)
	}
	if !(UsenetDownload{DownloadState: "failed"}).Failed() {
		t.Error("failed state should report Failed")
	}
	// TorBox appends a reason to the state string.
	repairFail := "failed (Repair failed, not enough repair blocks (73 short))"
	if !(UsenetDownload{DownloadState: repairFail}).Failed() {
		t.Error("repair-failure state should report Failed")
	}
	if !(UsenetDownload{DownloadState: "stalled (no seeds)"}).Failed() {
		t.Error("stalled state should report Failed")
	}
	if (UsenetDownload{DownloadState: "downloading"}).Failed() {
		t.Error("downloading should not report Failed")
	}
	if (UsenetDownload{DownloadState: "completed"}).Failed() {
		t.Error("completed should not report Failed")
	}
}

func TestETASeconds(t *testing.T) {
	if got := (UsenetDownload{ETA: 95.7}).ETASeconds(); got != 95 {
		t.Errorf("ETASeconds(95.7): got %d want 95", got)
	}
	if got := (UsenetDownload{ETA: -1}).ETASeconds(); got != 0 {
		t.Errorf("ETASeconds(-1): got %d want 0", got)
	}
	if got := (UsenetDownload{ETA: 0}).ETASeconds(); got != 0 {
		t.Errorf("ETASeconds(0): got %d want 0", got)
	}
}
