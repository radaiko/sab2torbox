package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/radaiko/sab2torbox/internal/job"
)

func TestQueueSlotFromJob(t *testing.T) {
	j := &job.Job{
		ID: 42, State: job.StateDownloading, Category: "sonarr",
		NZBName: "Show.S01E01", TotalBytes: 2 << 30, DownloadedBytes: 1 << 30,
		ProgressPct: 50, ETASeconds: 150,
	}
	slot := queueSlotFromJob(j)
	if slot.NzoID != "sab2tb_42" || slot.Filename != "Show.S01E01" {
		t.Errorf("bad slot: %+v", slot)
	}
	if slot.Status != "Downloading" || slot.Percentage != "50" {
		t.Errorf("bad status/pct: %+v", slot)
	}
	if slot.TimeLeft != "0:02:30" {
		t.Errorf("timeleft: got %q want 0:02:30", slot.TimeLeft)
	}
}

func TestFormatTimeLeft(t *testing.T) {
	cases := map[int64]string{
		0:     "0:00:00",
		-5:    "0:00:00",
		150:   "0:02:30",
		3661:  "1:01:01",
		90061: "1:01:01:01",
	}
	for in, want := range cases {
		if got := formatTimeLeft(in); got != want {
			t.Errorf("formatTimeLeft(%d): got %q want %q", in, got, want)
		}
	}
}

func TestHistorySlotFromJob(t *testing.T) {
	now := time.Now()
	j := &job.Job{
		ID: 7, State: job.StateCompleted, Category: "sonarr",
		NZBName: "Movie.2024", StoragePath: "/mnt/torbox/usenet/Movie.2024",
		TotalBytes: 1234, CompletedAt: &now,
	}
	slot := historySlotFromJob(j)
	if slot.NzoID != "sab2tb_7" || slot.Status != "Completed" {
		t.Errorf("bad slot: %+v", slot)
	}
	if slot.Storage != "/mnt/torbox/usenet/Movie.2024" || slot.Bytes != 1234 {
		t.Errorf("bad storage/bytes: %+v", slot)
	}
}

func TestHistorySlotFailed(t *testing.T) {
	j := &job.Job{ID: 9, State: job.StateFailed, NZBName: "x", FailMessage: "boom"}
	slot := historySlotFromJob(j)
	if slot.Status != "Failed" || slot.FailMessage != "boom" {
		t.Errorf("bad failed slot: %+v", slot)
	}
}

func TestResponsesMarshalShape(t *testing.T) {
	b, _ := json.Marshal(QueueResponse{Queue: Queue{Paused: false, Slots: []QueueSlot{}}})
	if string(b) != `{"queue":{"paused":false,"slots":[]}}` {
		t.Errorf("queue shape: %s", b)
	}
	b, _ = json.Marshal(VersionResponse{Version: "4.3.0"})
	if string(b) != `{"version":"4.3.0"}` {
		t.Errorf("version shape: %s", b)
	}
}
