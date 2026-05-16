// Package api implements the SABnzbd-compatible HTTP surface sab2torbox
// exposes to Sonarr and Radarr.
package api

import (
	"fmt"
	"strconv"

	"github.com/radaiko/sab2torbox/internal/job"
)

// VersionResponse answers mode=version.
type VersionResponse struct {
	Version string `json:"version"`
}

// AddResponse answers mode=addurl and mode=addfile.
type AddResponse struct {
	Status bool     `json:"status"`
	NzoIDs []string `json:"nzo_ids"`
}

// ErrorResponse is returned on authentication or request errors.
type ErrorResponse struct {
	Status bool   `json:"status"`
	Error  string `json:"error"`
}

// QueueSlot is one active download in the queue response.
type QueueSlot struct {
	NzoID      string `json:"nzo_id"`
	Filename   string `json:"filename"`
	Cat        string `json:"cat"`
	Status     string `json:"status"`
	MB         string `json:"mb"`
	MBLeft     string `json:"mbleft"`
	Percentage string `json:"percentage"`
	TimeLeft   string `json:"timeleft"`
}

// Queue is the body of a queue response.
type Queue struct {
	Paused bool        `json:"paused"`
	Slots  []QueueSlot `json:"slots"`
}

// QueueResponse answers mode=queue.
type QueueResponse struct {
	Queue Queue `json:"queue"`
}

// HistorySlot is one finished download in the history response.
type HistorySlot struct {
	NzoID       string `json:"nzo_id"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Status      string `json:"status"`
	Storage     string `json:"storage"`
	Bytes       int64  `json:"bytes"`
	FailMessage string `json:"fail_message"`
}

// History is the body of a history response.
type History struct {
	Slots []HistorySlot `json:"slots"`
}

// HistoryResponse answers mode=history.
type HistoryResponse struct {
	History History `json:"history"`
}

// Category is one entry in the get_config categories list.
type Category struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

// ConfigResponse answers mode=get_config.
type ConfigResponse struct {
	Config struct {
		Misc struct {
			CompleteDir string `json:"complete_dir"`
			DownloadDir string `json:"download_dir"`
		} `json:"misc"`
		Categories []Category `json:"categories"`
	} `json:"config"`
}

// StatusResponse answers mode=fullstatus.
type StatusResponse struct {
	Status struct {
		Paused bool `json:"paused"`
	} `json:"status"`
}

// DeleteResponse answers queue/history delete actions.
type DeleteResponse struct {
	Status bool `json:"status"`
}

// SymlinkHealthResponse answers GET /health/symlinks.
type SymlinkHealthResponse struct {
	Tracked    int64  `json:"tracked"`
	Broken     int64  `json:"broken"`
	Healing    int64  `json:"healing"`
	HealFailed int64  `json:"heal_failed"`
	LastRun    string `json:"last_run,omitempty"`
	NextRun    string `json:"next_run,omitempty"`
}

// HealFailedItem is one entry in the GET /health/heal_failed list.
type HealFailedItem struct {
	JobID          int64    `json:"job_id"`
	Name           string   `json:"name"`
	BrokenSymlinks []string `json:"broken_symlinks"`
	LastHealError  string   `json:"last_heal_error"`
	HealCount      int64    `json:"heal_count"`
	LastHealedAt   string   `json:"last_healed_at,omitempty"`
}

// queueStatusLabel maps a job state to the SAB queue status string.
func queueStatusLabel(s job.State) string {
	switch s {
	case job.StateDownloading, job.StateCompleted:
		return "Downloading"
	default:
		return "Queued"
	}
}

// queueSlotFromJob renders an in-progress job as a SAB queue slot.
func queueSlotFromJob(j *job.Job) QueueSlot {
	const mib = 1 << 20
	totalMB := float64(j.TotalBytes) / mib
	leftMB := float64(j.TotalBytes-j.DownloadedBytes) / mib
	if leftMB < 0 {
		leftMB = 0
	}
	return QueueSlot{
		NzoID:      j.NzoID(),
		Filename:   j.NZBName,
		Cat:        j.Category,
		Status:     queueStatusLabel(j.State),
		MB:         strconv.FormatFloat(totalMB, 'f', 1, 64),
		MBLeft:     strconv.FormatFloat(leftMB, 'f', 1, 64),
		Percentage: strconv.Itoa(j.ProgressPct),
		TimeLeft:   formatTimeLeft(j.ETASeconds),
	}
}

// formatTimeLeft renders seconds-remaining as the SABnzbd timeleft string
// (`H:MM:SS`, or `D:HH:MM:SS` once it exceeds a day). Sonarr parses this into
// the queue's estimated completion time.
func formatTimeLeft(seconds int64) string {
	if seconds <= 0 {
		return "0:00:00"
	}
	d := seconds / 86400
	h := (seconds % 86400) / 3600
	m := (seconds % 3600) / 60
	s := seconds % 60
	if d > 0 {
		return fmt.Sprintf("%d:%02d:%02d:%02d", d, h, m, s)
	}
	return fmt.Sprintf("%d:%02d:%02d", h, m, s)
}

// historySlotFromJob renders a finished job as a SAB history slot.
func historySlotFromJob(j *job.Job) HistorySlot {
	status := "Completed"
	if j.State == job.StateFailed {
		status = "Failed"
	}
	return HistorySlot{
		NzoID:       j.NzoID(),
		Name:        j.NZBName,
		Category:    j.Category,
		Status:      status,
		Storage:     j.StoragePath,
		Bytes:       j.TotalBytes,
		FailMessage: j.FailMessage,
	}
}
