// Package torbox is a client for the TorBox Usenet API.
package torbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// FlexInt is an int64 that decodes from a JSON number OR a JSON string.
// The TorBox API and its SDKs are inconsistent about whether IDs are
// serialized as numbers or strings, so every ID field uses this type.
type FlexInt int64

// UnmarshalJSON accepts a JSON number, a quoted numeric string, or null.
func (f *FlexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*f = 0
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			*f = 0
			return nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return fmt.Errorf("flexint from string %q: %w", s, err)
		}
		*f = FlexInt(n)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	i, err := n.Int64()
	if err != nil {
		// Tolerate floats like 7.0 by truncating.
		ff, ferr := n.Float64()
		if ferr != nil {
			return fmt.Errorf("flexint from number %q: %w", n, err)
		}
		*f = FlexInt(int64(ff))
		return nil
	}
	*f = FlexInt(i)
	return nil
}

// Envelope is the common TorBox response wrapper.
type Envelope struct {
	Success bool            `json:"success"`
	Error   json.RawMessage `json:"error"`
	Detail  string          `json:"detail"`
	Data    json.RawMessage `json:"data"`
}

// CreateResult is the data payload of a create-usenet-download response.
type CreateResult struct {
	UsenetDownloadID FlexInt `json:"usenetdownload_id"`
	Hash             string  `json:"hash"`
	AuthID           string  `json:"auth_id"`
}

// UsenetDownload is one record from the TorBox usenet/mylist endpoint.
type UsenetDownload struct {
	ID               FlexInt      `json:"id"`
	Hash             string       `json:"hash"`
	Name             string       `json:"name"`
	Size             int64        `json:"size"`
	DownloadState    string       `json:"download_state"`
	DownloadFinished bool         `json:"download_finished"`
	DownloadPresent  bool         `json:"download_present"`
	Progress         float64      `json:"progress"`
	DownloadSpeed    float64      `json:"download_speed"`
	ETA              float64      `json:"eta"`
	Active           bool         `json:"active"`
	Files            []UsenetFile `json:"files"`
}

// UsenetFile is one file inside a UsenetDownload.
type UsenetFile struct {
	ID        FlexInt `json:"id"`
	Name      string  `json:"name"`
	ShortName string  `json:"short_name"`
	Size      int64   `json:"size"`
	MimeType  string  `json:"mimetype"`
}

// ProgressPct converts the 0.0-1.0 progress float to a 0-100 integer.
func (d UsenetDownload) ProgressPct() int {
	p := d.Progress
	if p > 1 { // tolerate APIs that already return 0-100
		return int(p + 0.5)
	}
	return int(p*100 + 0.5)
}

// Failed reports whether the download is in a TorBox error/failed state.
// TorBox appends a human-readable reason to the state string, e.g.
// "failed (Repair failed, not enough repair blocks (73 short))", so the match
// is by prefix/keyword rather than an exact string comparison.
func (d UsenetDownload) Failed() bool {
	s := strings.ToLower(strings.TrimSpace(d.DownloadState))
	return strings.HasPrefix(s, "failed") ||
		strings.HasPrefix(s, "error") ||
		strings.Contains(s, "stalled")
}

// ETASeconds returns TorBox's estimated seconds remaining, clamped to >= 0.
func (d UsenetDownload) ETASeconds() int64 {
	if d.ETA <= 0 {
		return 0
	}
	return int64(d.ETA)
}

// DownloadedBytes estimates transferred bytes from progress and total size.
func (d UsenetDownload) DownloadedBytes() int64 {
	if d.Size <= 0 {
		return 0
	}
	p := d.Progress
	if p > 1 {
		p /= 100
	}
	return int64(float64(d.Size) * p)
}
