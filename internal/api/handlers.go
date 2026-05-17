package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/radaiko/sab2torbox/internal/config"
	"github.com/radaiko/sab2torbox/internal/job"
	"github.com/radaiko/sab2torbox/internal/store"
)

// maxNZBSize caps an uploaded NZB at 32 MiB.
const maxNZBSize = 32 << 20

// Checker reports service health. api.Health satisfies it.
type Checker interface {
	Check(ctx context.Context) error
}

// HealReporter exposes the healer's schedule for the /health/symlinks endpoint.
type HealReporter interface {
	HealRunInfo() (last, next time.Time)
}

// Server holds dependencies for the SABnzbd-compatible HTTP API.
type Server struct {
	store        *store.Store
	cfg          *config.Config
	logger       *slog.Logger
	health       Checker
	healReporter HealReporter
}

// NewServer constructs a Server.
func NewServer(st *store.Store, cfg *config.Config, logger *slog.Logger) *Server {
	return &Server{store: st, cfg: cfg, logger: logger}
}

// SetHealth attaches a health checker for the /healthz endpoint.
func (s *Server) SetHealth(c Checker) { s.health = c }

// SetHealReporter attaches the healer's status source for /health/symlinks.
func (s *Server) SetHealReporter(r HealReporter) { s.healReporter = r }

// Router builds the chi router with all routes.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", s.handleHealthz)
	r.Get("/health/symlinks", s.handleHealthSymlinks)
	r.Get("/health/heal_failed", s.handleHealFailed)
	r.Post("/health/heal/{jobID}/retry", s.handleHealRetry)
	r.Post("/health/heal/{jobID}/give_up", s.handleHealGiveUp)
	for _, base := range []string{"/api", "/sabnzbd/api"} {
		r.Get(base, s.handleAPI)
		r.Post(base, s.handleAPI)
	}
	return r
}

// writeJSON marshals v as the HTTP response body.
func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.logger.Error("encoding response", "error", err)
	}
}

// validAPIKey reports whether got matches want, in constant time.
func validAPIKey(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// param reads a parameter from the query string, falling back to POST form.
func param(r *http.Request, key string) string {
	if v := r.URL.Query().Get(key); v != "" {
		return v
	}
	return r.PostFormValue(key)
}

// handleAPI authenticates and dispatches by the SAB mode parameter.
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	// ParseForm for non-multipart bodies; multipart handled per-mode.
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		_ = r.ParseForm()
	}
	if !validAPIKey(param(r, "apikey"), s.cfg.SABAPIKey) {
		s.writeJSON(w, ErrorResponse{Status: false, Error: "API Key Incorrect"})
		return
	}
	mode := param(r, "mode")
	switch mode {
	case "version":
		s.writeJSON(w, VersionResponse{Version: "4.3.0"})
	case "get_config":
		s.handleGetConfig(w)
	case "fullstatus":
		s.writeJSON(w, StatusResponse{})
	case "addurl":
		s.handleAddURL(w, r)
	case "addfile":
		s.handleAddFile(w, r)
	case "queue":
		s.handleQueue(w, r)
	case "history":
		s.handleHistory(w, r)
	default:
		s.writeJSON(w, ErrorResponse{Status: false, Error: "not implemented: mode=" + mode})
	}
}

func (s *Server) handleGetConfig(w http.ResponseWriter) {
	var resp ConfigResponse
	// Releases are published under <SymlinkRoot>/<category>/, so each
	// category maps to a real subdirectory (pre-created at startup) that
	// Sonarr/Radarr can health-check.
	resp.Config.Misc.CompleteDir = s.cfg.SymlinkRoot
	resp.Config.Misc.DownloadDir = s.cfg.SymlinkRoot
	resp.Config.Categories = []Category{{Name: "*", Dir: ""}}
	for _, c := range s.cfg.Categories {
		resp.Config.Categories = append(resp.Config.Categories, Category{Name: c, Dir: c})
	}
	s.writeJSON(w, resp)
}

// handleAddURL submits an NZB referenced by URL.
func (s *Server) handleAddURL(w http.ResponseWriter, r *http.Request) {
	nzbURL := param(r, "name")
	cat := param(r, "cat")
	name := param(r, "nzbname")
	if name == "" {
		name = nzbURL
	}
	if nzbURL == "" {
		s.writeJSON(w, ErrorResponse{Status: false, Error: "missing nzb url"})
		return
	}
	ctx := r.Context()
	if existing, _ := s.store.FindByURL(ctx, nzbURL, cat); existing != nil {
		s.writeJSON(w, AddResponse{Status: true, NzoIDs: []string{existing.NzoID()}})
		return
	}
	j := &job.Job{State: job.StatePending, Category: cat, NZBName: name, NZBURL: nzbURL}
	s.createAndRespond(w, ctx, j)
}

// handleAddFile submits an uploaded NZB file.
func (s *Server) handleAddFile(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxNZBSize); err != nil {
		s.writeJSON(w, ErrorResponse{Status: false, Error: "invalid multipart form"})
		return
	}
	cat := param(r, "cat")
	name := param(r, "nzbname")
	file, header, err := r.FormFile("name")
	if err != nil {
		s.writeJSON(w, ErrorResponse{Status: false, Error: "missing nzb file"})
		return
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, maxNZBSize))
	if err != nil {
		s.writeJSON(w, ErrorResponse{Status: false, Error: "reading nzb file"})
		return
	}
	if name == "" {
		name = strings.TrimSuffix(header.Filename, ".nzb")
	}
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])

	ctx := r.Context()
	if existing, _ := s.store.FindBySHA256(ctx, sha, cat); existing != nil {
		s.writeJSON(w, AddResponse{Status: true, NzoIDs: []string{existing.NzoID()}})
		return
	}
	j := &job.Job{
		State: job.StatePending, Category: cat, NZBName: name,
		NZBContent: content, NZBSHA256: sha,
	}
	s.createAndRespond(w, ctx, j)
}

// createAndRespond inserts j and writes the SAB add response.
func (s *Server) createAndRespond(w http.ResponseWriter, ctx context.Context, j *job.Job) {
	id, err := s.store.CreateJob(ctx, j)
	if err != nil {
		s.logger.Error("creating job", "error", err)
		s.writeJSON(w, ErrorResponse{Status: false, Error: "internal error"})
		return
	}
	j.ID = id
	s.logger.Info("job accepted", "job_id", id, "nzb_name", j.NZBName, "category", j.Category)
	s.writeJSON(w, AddResponse{Status: true, NzoIDs: []string{j.NzoID()}})
}

// handleQueue answers mode=queue and its delete action.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	if param(r, "name") == "delete" {
		s.handleDelete(w, r)
		return
	}
	jobs, err := s.store.JobsByState(r.Context(),
		job.StatePending, job.StateSubmitting, job.StateQueued, job.StateDownloading)
	if err != nil {
		s.logger.Error("loading queue", "error", err)
		jobs = nil
	}
	slots := make([]QueueSlot, 0, len(jobs))
	for _, j := range jobs {
		slots = append(slots, queueSlotFromJob(j))
	}
	s.writeJSON(w, QueueResponse{Queue: Queue{Paused: false, Slots: slots}})
}

// handleHistory answers mode=history and its delete action.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if param(r, "name") == "delete" {
		s.handleDelete(w, r)
		return
	}
	// Only completed and failed jobs are surfaced to Sonarr. Imported jobs
	// are deliberately omitted: once Sonarr has moved a release out of the
	// symlink farm its per-release directory is swept away, so advertising it
	// here with a storage path that no longer exists makes Sonarr retry the
	// import forever ("path does not exist or is not accessible").
	jobs, err := s.store.JobsByState(r.Context(),
		job.StateCompleted, job.StateFailed)
	if err != nil {
		s.logger.Error("loading history", "error", err)
		jobs = nil
	}
	slots := make([]HistorySlot, 0, len(jobs))
	for _, j := range jobs {
		slots = append(slots, historySlotFromJob(j))
	}
	s.writeJSON(w, HistoryResponse{History: History{Slots: slots}})
}

// handleDelete removes jobs named by the comma-separated nzo_id list in
// value=. With del_files=1 the job is marked for deletion and the deleter
// worker removes the download from TorBox (with retries); without it, the
// local row is simply dropped and the TorBox download is left in place.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	delFiles := param(r, "del_files") == "1"
	for _, raw := range strings.Split(param(r, "value"), ",") {
		id, ok := parseNzoID(strings.TrimSpace(raw))
		if !ok {
			continue
		}
		j, err := s.store.GetJob(ctx, id)
		if err != nil {
			continue
		}
		if delFiles && j.TorBoxID != 0 {
			// Hand off to the deleter worker: it removes the download from
			// TorBox (retrying transient failures) and then drops the row.
			j.State = job.StateDeleted
			if err := s.store.UpdateJob(ctx, j); err != nil {
				s.logger.Error("marking job for deletion", "job_id", id, "error", err)
			} else {
				s.logger.Info("job queued for torbox deletion",
					"job_id", id, "torbox_id", j.TorBoxID)
			}
			continue
		}
		if err := s.store.DeleteJob(ctx, id); err != nil {
			s.logger.Error("deleting job row", "job_id", id, "error", err)
		} else {
			s.logger.Info("job deleted", "job_id", id, "del_files", delFiles)
		}
	}
	s.writeJSON(w, DeleteResponse{Status: true})
}

// parseNzoID extracts the numeric job ID from a "sab2tb_<id>" string.
func parseNzoID(nzo string) (int64, bool) {
	digits := strings.TrimPrefix(nzo, "sab2tb_")
	if digits == nzo {
		return 0, false
	}
	id, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// handleHealthSymlinks answers GET /health/symlinks with heal/symlink counts.
func (s *Server) handleHealthSymlinks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var resp SymlinkHealthResponse
	tracked, broken, err := s.store.SymlinkCounts(ctx)
	if err != nil {
		s.logger.Error("counting symlinks", "error", err)
	}
	resp.Tracked, resp.Broken = tracked, broken
	if n, err := s.store.CountJobsByState(ctx, job.StateHealing); err == nil {
		resp.Healing = n
	}
	if n, err := s.store.CountJobsByState(ctx, job.StateHealFailed); err == nil {
		resp.HealFailed = n
	}
	if s.healReporter != nil {
		last, next := s.healReporter.HealRunInfo()
		if !last.IsZero() {
			resp.LastRun = last.UTC().Format(time.RFC3339)
		}
		if !next.IsZero() {
			resp.NextRun = next.UTC().Format(time.RFC3339)
		}
	}
	s.writeJSON(w, resp)
}

// handleHealFailed lists jobs the healer has given up on (heal_count has
// reached HEAL_MAX_ATTEMPTS), with their broken symlinks.
func (s *Server) handleHealFailed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jobs, err := s.store.JobsByState(ctx, job.StateHealFailed)
	if err != nil {
		s.logger.Error("loading heal_failed jobs", "error", err)
	}
	syms, err := s.store.ListImportedSymlinks(ctx)
	if err != nil {
		s.logger.Error("loading symlinks for heal_failed", "error", err)
	}
	brokenByJob := make(map[int64][]string)
	for _, sym := range syms {
		if sym.IsBroken {
			brokenByJob[sym.JobID] = append(brokenByJob[sym.JobID], sym.SymlinkPath)
		}
	}
	items := make([]HealFailedItem, 0)
	for _, j := range jobs {
		if int(j.HealCount) < s.cfg.HealMaxAttempts {
			continue // still being retried — not stuck
		}
		item := HealFailedItem{
			JobID:          j.ID,
			Name:           j.NZBName,
			BrokenSymlinks: brokenByJob[j.ID],
			LastHealError:  j.LastHealError,
			HealCount:      j.HealCount,
		}
		if item.BrokenSymlinks == nil {
			item.BrokenSymlinks = []string{}
		}
		if j.LastHealedAt != nil {
			item.LastHealedAt = j.LastHealedAt.UTC().Format(time.RFC3339)
		}
		items = append(items, item)
	}
	s.writeJSON(w, items)
}

// handleHealRetry resets a job's heal attempts so the healer retries it.
func (s *Server) handleHealRetry(w http.ResponseWriter, r *http.Request) {
	j, ok := s.healJobFromURL(w, r)
	if !ok {
		return
	}
	if j.State != job.StateHealFailed {
		s.writeJSON(w, ErrorResponse{Status: false,
			Error: "job is not in heal_failed state"})
		return
	}
	j.HealCount = 0
	j.LastHealedAt = nil
	j.LastHealError = ""
	if err := s.store.UpdateJob(r.Context(), j); err != nil {
		s.logger.Error("heal retry: updating job", "job_id", j.ID, "error", err)
		s.writeJSON(w, ErrorResponse{Status: false, Error: "internal error"})
		return
	}
	s.logger.Info("heal retry requested", "job_id", j.ID)
	s.writeJSON(w, DeleteResponse{Status: true})
}

// handleHealGiveUp marks a job manually_resolved and stops tracking its
// symlinks, so the healer ignores it.
func (s *Server) handleHealGiveUp(w http.ResponseWriter, r *http.Request) {
	j, ok := s.healJobFromURL(w, r)
	if !ok {
		return
	}
	if j.State != job.StateHealFailed {
		s.writeJSON(w, ErrorResponse{Status: false,
			Error: "job is not in heal_failed state"})
		return
	}
	ctx := r.Context()
	j.State = job.StateManuallyResolved
	if err := s.store.UpdateJob(ctx, j); err != nil {
		s.logger.Error("heal give_up: updating job", "job_id", j.ID, "error", err)
		s.writeJSON(w, ErrorResponse{Status: false, Error: "internal error"})
		return
	}
	if err := s.store.DeleteImportedSymlinksByJob(ctx, j.ID); err != nil {
		s.logger.Error("heal give_up: dropping symlinks", "job_id", j.ID, "error", err)
	}
	s.logger.Info("heal given up", "job_id", j.ID)
	s.writeJSON(w, DeleteResponse{Status: true})
}

// healJobFromURL loads the job named by the {jobID} URL parameter, writing an
// error response and returning ok=false if it is missing or unparseable.
func (s *Server) healJobFromURL(w http.ResponseWriter, r *http.Request) (*job.Job, bool) {
	// These endpoints mutate job state, so they require the SAB API key.
	if !validAPIKey(param(r, "apikey"), s.cfg.SABAPIKey) {
		s.writeJSON(w, ErrorResponse{Status: false, Error: "API Key Incorrect"})
		return nil, false
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "jobID"), 10, 64)
	if err != nil {
		s.writeJSON(w, ErrorResponse{Status: false, Error: "invalid job id"})
		return nil, false
	}
	j, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		s.writeJSON(w, ErrorResponse{Status: false, Error: "job not found"})
		return nil, false
	}
	return j, true
}

// handleHealthz answers the /healthz probe.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if s.health == nil {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	if err := s.health.Check(r.Context()); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unhealthy: " + err.Error()))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
