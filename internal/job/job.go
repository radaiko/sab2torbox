// Package job defines the sab2torbox domain record and its state machine.
package job

import "time"

// State is a job lifecycle stage.
type State string

// Job lifecycle states.
const (
	StatePending     State = "pending"     // received from Sonarr, not yet sent to TorBox
	StateSubmitting  State = "submitting"  // submit to TorBox in progress
	StateQueued      State = "queued"      // accepted by TorBox, not yet transferring
	StateDownloading State = "downloading" // TorBox is transferring
	StateCompleted   State = "completed"   // finished and present; storage path resolved
	StateImported    State = "imported"    // Sonarr has read the history entry
	StateDeleted     State = "deleted"     // removed from TorBox at Sonarr's request
	StateFailed      State = "failed"      // terminal error
)

// transitions lists the allowed next states for each state.
var transitions = map[State][]State{
	StatePending:     {StateSubmitting, StateFailed},
	StateSubmitting:  {StateQueued, StatePending, StateFailed},
	StateQueued:      {StateDownloading, StateCompleted, StateFailed},
	StateDownloading: {StateCompleted, StateFailed},
	StateCompleted:   {StateImported, StateDeleted, StateFailed},
	StateImported:    {StateDeleted, StateFailed},
	StateDeleted:     {},
	StateFailed:      {},
}

// CanTransitionTo reports whether moving from s to next is allowed.
func (s State) CanTransitionTo(next State) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// IsTerminal reports whether s is an end state with no outgoing transitions.
func (s State) IsTerminal() bool {
	return len(transitions[s]) == 0
}

// Job is the persisted record of one NZB submission.
type Job struct {
	ID              int64
	State           State
	Category        string
	NZBName         string
	NZBContent      []byte
	NZBURL          string
	NZBSHA256       string
	TorBoxID        int64
	TorBoxHash      string
	StoragePath     string
	TotalBytes      int64
	DownloadedBytes int64
	ProgressPct     int
	ETASeconds      int64
	FailMessage     string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	SubmittedAt     *time.Time
	CompletedAt     *time.Time
}

// NzoID returns the SABnzbd nzo_id Sonarr uses to reference this job.
func (j *Job) NzoID() string { return "sab2tb_" + itoa(j.ID) }

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
