// Package api implements the SABnzbd-compatible HTTP surface sab2torbox
// exposes to Sonarr and Radarr.
package api

import (
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
		TimeLeft:   "0:00:00",
	}
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
