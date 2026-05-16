# Auto-Heal Symlinks — Phase 2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Round out the auto-healing feature with operator-facing controls — a webhook on heal events, a `/health/heal_failed` listing, retry/give-up endpoints, the `manually_resolved` terminal state — and fold in the three minor fixes from the Phase 1 final review.

**Architecture:** Phase 1's healer already discovers, detects, resubmits, and reconciles. Phase 2 adds: an opt-in best-effort webhook fired from the healer's lifecycle points (`detected`/`healing`/`healed`/`failed`); three HTTP endpoints under `/health/heal*` for inspecting and overriding stuck heals; and a `manually_resolved` state the `give_up` endpoint sets so the healer ignores a job the operator has taken over.

**Tech Stack:** Go 1.25, existing `internal/worker` + `internal/api` packages, `chi` router, `modernc.org/sqlite`.

**Builds on:** Phase 1 (merged to `main`). Assumes states `healing`/`heal_failed`, the `imported_symlinks` table, `HEAL_*` config, the healer (`healer.go`), and `GET /health/symlinks` all exist.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/job/job.go` | Add `manually_resolved` state + transition |
| `internal/config/config.go` | `HEAL_WEBHOOK_*` config; validate `HEAL_MAX_ATTEMPTS > 0` |
| `internal/worker/webhook.go` | New: heal webhook payload + sender |
| `internal/worker/healer.go` | Fire webhook events; fix the startHeal double-resubmit guard |
| `internal/store/store.go` | `DeleteImportedSymlinksByJob` |
| `internal/api/responses.go` | `HealFailedItem` response; `omitempty` on `SymlinkHealthResponse` times |
| `internal/api/handlers.go` | `/health/heal_failed`, `/health/heal/{jobID}/retry`, `.../give_up` |
| `README.md`, `.env.example` | Document the webhook + endpoints |

---

## Task 1: The `manually_resolved` state

**Files:**
- Modify: `internal/job/job.go`
- Modify: `internal/job/job_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/job/job_test.go`:

```go
func TestManuallyResolvedTransitions(t *testing.T) {
	if !StateHealFailed.CanTransitionTo(StateManuallyResolved) {
		t.Error("heal_failed must be able to transition to manually_resolved")
	}
	if !StateManuallyResolved.IsTerminal() {
		t.Error("manually_resolved must be terminal")
	}
	if StateImported.CanTransitionTo(StateManuallyResolved) {
		t.Error("imported must not transition straight to manually_resolved")
	}
}
```

- [ ] **Step 2: Run it — `go test ./internal/job/` — expect FAIL** (undefined `StateManuallyResolved`).

- [ ] **Step 3: Add the state**

In `internal/job/job.go`, add to the state constants block (after `StateHealFailed`):

```go
	StateManuallyResolved State = "manually_resolved" // operator gave up on healing; healer ignores it
```

In the `transitions` map, replace the `StateHealFailed` entry and add `StateManuallyResolved`:

```go
	StateHealFailed:       {StateHealing, StateManuallyResolved},
	StateManuallyResolved: {},
```

- [ ] **Step 4: Run it — `go test ./internal/job/` — expect PASS.**

- [ ] **Step 5: Commit**

```bash
git add internal/job && git commit -m "feat: add manually_resolved heal state"
```

---

## Task 2: Phase 1 review fixes

Three minor issues from the Phase 1 final review, plus a missing dry-run test.

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/api/responses.go`
- Modify: `internal/worker/healer.go`
- Modify: `internal/worker/worker_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
func TestHealRejectsZeroMaxAttempts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAB2TORBOX_TORBOX_API_TOKEN", "t")
	t.Setenv("SAB2TORBOX_SAB_API_KEY", "k")
	t.Setenv("SAB2TORBOX_WEBDAV_MOUNT_ROOT", dir)
	t.Setenv("SAB2TORBOX_SYMLINK_ROOT", dir)
	t.Setenv("SAB2TORBOX_HEAL_ENABLED", "true")
	t.Setenv("SAB2TORBOX_HEAL_LIBRARY_ROOTS", dir)
	t.Setenv("SAB2TORBOX_HEAL_MAX_ATTEMPTS", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected error: HEAL_MAX_ATTEMPTS=0 means nothing ever heals")
	}
}
```

Append to `internal/worker/worker_test.go`:

```go
func TestHealerDryRunDoesNotResubmit(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, cfg := testWorkers(t, fake)
	cfg.HealMaxAttempts = 3
	cfg.HealDryRun = true
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StateImported, Category: "c", NZBName: "n", NZBContent: []byte("x"),
	})
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: id, SymlinkPath: "/lib/x.mkv", TargetPath: "/mnt/torbox/N/x.mkv",
	})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())

	if err := w.triggerHeals(ctx); err != nil {
		t.Fatalf("triggerHeals: %v", err)
	}
	if len(fake.created) != 0 {
		t.Error("dry-run must not resubmit anything to TorBox")
	}
	if got, _ := st.GetJob(ctx, id); got.State != job.StateImported {
		t.Errorf("dry-run must leave the job state unchanged, got %s", got.State)
	}
}
```

- [ ] **Step 2: Run both — `go test ./internal/config/ ./internal/worker/` — expect FAIL** (`TestHealRejectsZeroMaxAttempts` fails; `TestHealerDryRunDoesNotResubmit` should actually already pass if Phase 1's dry-run works — if it passes, that is fine, it is a regression guard).

- [ ] **Step 3: Add the `HEAL_MAX_ATTEMPTS` validation**

In `internal/config/config.go`, inside `Load()`, in the existing `if c.HealEnabled { ... }` block, after the `HealLibraryRoots` checks, add:

```go
		if c.HealMaxAttempts <= 0 {
			return nil, fmt.Errorf("HEAL_MAX_ATTEMPTS must be greater than 0")
		}
```

- [ ] **Step 4: Add `omitempty` to the symlink-health times**

In `internal/api/responses.go`, in `SymlinkHealthResponse`, change the `LastRun` and `NextRun` json tags to add `,omitempty`:

```go
	LastRun    string `json:"last_run,omitempty"`
	NextRun    string `json:"next_run,omitempty"`
```

- [ ] **Step 5: Guard against double-resubmission in `startHeal`**

In `internal/worker/healer.go`, in `startHeal`, the success path currently ends with:

```go
	if err := w.store.UpdateJob(ctx, j); err != nil {
		log.Error("heal: persisting healing state", "error", err)
		return
	}
```

Replace that block with one that marks the job failed if the state can't be persisted — otherwise the job stays `imported`, is re-eligible next cycle, and resubmits a duplicate download:

```go
	if err := w.store.UpdateJob(ctx, j); err != nil {
		log.Error("heal: persisting healing state, marking failed to avoid a duplicate resubmit",
			"error", err)
		w.markHealFailed(ctx, j, "persisting healing state failed: "+err.Error())
		return
	}
```

- [ ] **Step 6: Run the tests — `go test ./internal/config/ ./internal/worker/ ./internal/api/` — expect PASS.**

- [ ] **Step 7: Commit**

```bash
git add internal/config internal/api internal/worker && git commit -m "fix: address Phase 1 review (max-attempts validation, omitempty, resubmit guard)"
```

---

## Task 3: Heal webhook sender

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Create: `internal/worker/webhook.go`
- Modify: `internal/worker/worker_test.go`

- [ ] **Step 1: Write the failing config test**

Append to `internal/config/config_test.go`:

```go
func TestHealWebhookDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAB2TORBOX_TORBOX_API_TOKEN", "t")
	t.Setenv("SAB2TORBOX_SAB_API_KEY", "k")
	t.Setenv("SAB2TORBOX_WEBDAV_MOUNT_ROOT", dir)
	t.Setenv("SAB2TORBOX_SYMLINK_ROOT", dir)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HealWebhookURL != "" {
		t.Errorf("HealWebhookURL default should be empty, got %q", c.HealWebhookURL)
	}
	if len(c.HealWebhookEvents) != 1 || c.HealWebhookEvents[0] != "failed" {
		t.Errorf("HealWebhookEvents default should be [failed], got %v", c.HealWebhookEvents)
	}
}
```

- [ ] **Step 2: Run it — `go test ./internal/config/` — expect FAIL** (undefined `HealWebhookURL`).

- [ ] **Step 3: Add the webhook config fields**

In `internal/config/config.go`, add to the `Config` struct after `HealBackoffInitial`:

```go
	HealWebhookURL    string   `envconfig:"HEAL_WEBHOOK_URL"`
	HealWebhookEvents []string `envconfig:"HEAL_WEBHOOK_EVENTS" default:"failed"`
```

- [ ] **Step 4: Run it — `go test ./internal/config/` — expect PASS.**

- [ ] **Step 5: Write the failing webhook test**

Append to `internal/worker/worker_test.go`:

```go
func TestHealWebhookWants(t *testing.T) {
	events := []string{"failed", "healed"}
	if !healWebhookWants(events, "failed") {
		t.Error("failed should be wanted")
	}
	if healWebhookWants(events, "detected") {
		t.Error("detected should not be wanted")
	}
}

func TestEmitHealEventPostsPayload(t *testing.T) {
	received := make(chan webhookPayload, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type: %q", r.Header.Get("Content-Type"))
		}
		var p webhookPayload
		json.NewDecoder(r.Body).Decode(&p)
		received <- p
	}))
	defer srv.Close()

	w, _, cfg := testWorkers(t, &fakeTorBox{})
	cfg.HealWebhookURL = srv.URL
	cfg.HealWebhookEvents = []string{"healed"}

	j := &job.Job{ID: 7, NZBName: "Rel", Category: "sonarr", HealCount: 1}
	w.emitHealEvent("healed", j, healEventExtra{SymlinksHealed: 2, NewTorBoxID: 99})

	select {
	case p := <-received:
		if p.Event != "healed" || p.Job.ID != 7 || p.SymlinksHealed != 2 || p.NewTorBoxID != 99 {
			t.Errorf("bad payload: %+v", p)
		}
		if p.Timestamp == "" {
			t.Error("timestamp must be set")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook was not delivered")
	}
}

func TestEmitHealEventSkipsUnwantedAndUnconfigured(t *testing.T) {
	hits := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
	}))
	defer srv.Close()

	w, _, cfg := testWorkers(t, &fakeTorBox{})
	j := &job.Job{ID: 1, NZBName: "n"}

	// Unconfigured URL: no POST.
	w.emitHealEvent("failed", j, healEventExtra{})
	// Configured, but the event is not in the wanted set: no POST.
	cfg.HealWebhookURL = srv.URL
	cfg.HealWebhookEvents = []string{"healed"}
	w.emitHealEvent("failed", j, healEventExtra{})

	select {
	case <-hits:
		t.Fatal("webhook fired when it should not have")
	case <-time.After(300 * time.Millisecond):
		// expected — nothing delivered
	}
}
```

The webhook tests use `encoding/json`, `net/http`, and `net/http/httptest` — `net/http` and `httptest` are already imported in `worker_test.go`, but **add `"encoding/json"` to the `worker_test.go` import block if it is not already present**.

- [ ] **Step 6: Run it — `go test ./internal/worker/ -run 'HealWebhook|EmitHeal'` — expect FAIL** (undefined `healWebhookWants`).

- [ ] **Step 7: Create `internal/worker/webhook.go`**

```go
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/radaiko/sab2torbox/internal/job"
)

// webhookJob is the job summary embedded in a heal webhook payload.
type webhookJob struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Category  string `json:"category"`
	HealCount int64  `json:"heal_count"`
}

// webhookPayload is the JSON body POSTed to HealWebhookURL on a heal event.
type webhookPayload struct {
	Event          string     `json:"event"`
	Timestamp      string     `json:"timestamp"`
	Job            webhookJob `json:"job"`
	SymlinksHealed int        `json:"symlinks_healed,omitempty"`
	OldTorBoxID    int64      `json:"old_torbox_id,omitempty"`
	NewTorBoxID    int64      `json:"new_torbox_id,omitempty"`
	Error          string     `json:"error,omitempty"`
}

// healEventExtra carries the event-specific optional fields a caller supplies.
type healEventExtra struct {
	SymlinksHealed int
	OldTorBoxID    int64
	NewTorBoxID    int64
	Error          string
}

// healWebhookWants reports whether event is in the configured event set.
func healWebhookWants(events []string, event string) bool {
	for _, e := range events {
		if e == event {
			return true
		}
	}
	return false
}

// emitHealEvent fires a heal webhook for event, if a webhook URL is configured
// and event is in HealWebhookEvents. The POST runs in its own goroutine so a
// slow or unreachable webhook endpoint never stalls the healer.
func (w *Workers) emitHealEvent(event string, j *job.Job, extra healEventExtra) {
	if w.cfg.HealWebhookURL == "" || !healWebhookWants(w.cfg.HealWebhookEvents, event) {
		return
	}
	payload := webhookPayload{
		Event:     event,
		Timestamp: timeNow().UTC().Format(time.RFC3339),
		Job: webhookJob{
			ID: j.ID, Name: j.NZBName, Category: j.Category, HealCount: j.HealCount,
		},
		SymlinksHealed: extra.SymlinksHealed,
		OldTorBoxID:    extra.OldTorBoxID,
		NewTorBoxID:    extra.NewTorBoxID,
		Error:          extra.Error,
	}
	go w.postHealWebhook(payload)
}

// postHealWebhook delivers one webhook payload, best-effort. Failures are
// logged, never retried — the webhook is a notification, not a guarantee.
func (w *Workers) postHealWebhook(payload webhookPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		w.logger.Error("heal webhook: encoding payload", "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.HealWebhookURL,
		bytes.NewReader(body))
	if err != nil {
		w.logger.Error("heal webhook: building request", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.httpClient.Do(req)
	if err != nil {
		w.logger.Warn("heal webhook: post failed", "event", payload.Event, "error", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		w.logger.Warn("heal webhook: non-2xx response",
			"event", payload.Event, "status", resp.StatusCode)
	}
}
```

- [ ] **Step 8: Run it — `go test ./internal/worker/ -run 'HealWebhook|EmitHeal'` — expect PASS.**

- [ ] **Step 9: Commit**

```bash
git add internal/config internal/worker && git commit -m "feat: add heal webhook sender"
```

---

## Task 4: Fire webhook events from the healer

**Files:**
- Modify: `internal/worker/healer.go`
- Modify: `internal/worker/worker_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/worker/worker_test.go`:

```go
func TestHealerFiresFailedEvent(t *testing.T) {
	received := make(chan webhookPayload, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p webhookPayload
		json.NewDecoder(r.Body).Decode(&p)
		received <- p
	}))
	defer srv.Close()

	fake := &fakeTorBox{createErr: &torbox.APIError{Status: 500, Detail: "down"}}
	w, st, cfg := testWorkers(t, fake)
	cfg.HealMaxAttempts = 3
	cfg.HealWebhookURL = srv.URL
	cfg.HealWebhookEvents = []string{"detected", "failed"}
	ctx := context.Background()

	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StateImported, Category: "c", NZBName: "n", NZBContent: []byte("x"),
	})
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: id, SymlinkPath: "/lib/x.mkv", TargetPath: "/mnt/torbox/N/x.mkv",
	})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())

	if err := w.triggerHeals(ctx); err != nil {
		t.Fatalf("triggerHeals: %v", err)
	}
	// Expect a "detected" and a "failed" event (order not guaranteed).
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case p := <-received:
			seen[p.Event] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("missing webhook events; got %v", seen)
		}
	}
	if !seen["detected"] || !seen["failed"] {
		t.Errorf("expected detected+failed events, got %v", seen)
	}
}
```

- [ ] **Step 2: Run it — `go test ./internal/worker/ -run FiresFailedEvent` — expect FAIL** (no events emitted yet).

- [ ] **Step 3: Wire the events into the healer**

In `internal/worker/healer.go`:

In `startHeal`, immediately after the `log := w.logger.With(...)` line at the top, add the `detected` event:

```go
	w.emitHealEvent("detected", j, healEventExtra{})
```

In `startHeal`, on the success path, after the `UpdateJob` succeeds and before the final `log.Info(...)`, add the `healing` event:

```go
	w.emitHealEvent("healing", j, healEventExtra{NewTorBoxID: j.TorBoxID})
```

In `markHealFailed`, after the `UpdateJob` call (at the end of the function), add the `failed` event:

```go
	w.emitHealEvent("failed", j, healEventExtra{Error: msg})
```

In `finishHeal`, on the success path, after the `UpdateJob` succeeds and before the final `log.Info(...)`, add the `healed` event:

```go
	w.emitHealEvent("healed", j, healEventExtra{SymlinksHealed: healed, NewTorBoxID: j.TorBoxID})
```

- [ ] **Step 4: Run it — `go test ./internal/worker/ -run FiresFailedEvent` and the full package `go test ./internal/worker/ -count=1` — expect PASS.**

- [ ] **Step 5: Commit**

```bash
git add internal/worker && git commit -m "feat: fire heal webhook events from the healer lifecycle"
```

---

## Task 5: Store — delete symlinks by job

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/store/store_test.go`:

```go
func TestDeleteImportedSymlinksByJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	jobA, _ := s.CreateJob(ctx, &job.Job{State: job.StateImported, Category: "c", NZBName: "a"})
	jobB, _ := s.CreateJob(ctx, &job.Job{State: job.StateImported, Category: "c", NZBName: "b"})
	s.UpsertImportedSymlink(ctx, &job.ImportedSymlink{JobID: jobA, SymlinkPath: "/l/a1", TargetPath: "/t/a1"})
	s.UpsertImportedSymlink(ctx, &job.ImportedSymlink{JobID: jobA, SymlinkPath: "/l/a2", TargetPath: "/t/a2"})
	s.UpsertImportedSymlink(ctx, &job.ImportedSymlink{JobID: jobB, SymlinkPath: "/l/b1", TargetPath: "/t/b1"})

	if err := s.DeleteImportedSymlinksByJob(ctx, jobA); err != nil {
		t.Fatalf("DeleteImportedSymlinksByJob: %v", err)
	}
	list, _ := s.ListImportedSymlinks(ctx)
	if len(list) != 1 || list[0].JobID != jobB {
		t.Errorf("only job B's symlink should remain, got %+v", list)
	}
}
```

- [ ] **Step 2: Run it — `go test ./internal/store/` — expect FAIL** (undefined `DeleteImportedSymlinksByJob`).

- [ ] **Step 3: Add the method**

Append to `internal/store/store.go`:

```go
// DeleteImportedSymlinksByJob removes every tracked symlink belonging to a job.
func (s *Store) DeleteImportedSymlinksByJob(ctx context.Context, jobID int64) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM imported_symlinks WHERE job_id=?`, jobID); err != nil {
		return fmt.Errorf("deleting symlinks for job %d: %w", jobID, err)
	}
	return nil
}
```

- [ ] **Step 4: Run it — `go test ./internal/store/` — expect PASS.**

- [ ] **Step 5: Commit**

```bash
git add internal/store && git commit -m "feat: add DeleteImportedSymlinksByJob"
```

---

## Task 6: Heal management endpoints

`GET /health/heal_failed`, `POST /health/heal/{jobID}/retry`, `POST /health/heal/{jobID}/give_up`.

**Files:**
- Modify: `internal/api/responses.go`
- Modify: `internal/api/handlers.go`
- Modify: `internal/api/handlers_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/api/handlers_test.go`:

```go
func TestHealFailedEndpoint(t *testing.T) {
	srv, st := testServer(t)
	srv.cfg.HealMaxAttempts = 3
	ctx := context.Background()

	// An exhausted job (heal_count >= max) — should be listed.
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealFailed, Category: "sonarr", NZBName: "Dead"})
	j, _ := st.GetJob(ctx, id)
	j.HealCount = 3
	j.LastHealError = "nzb gone"
	st.UpdateJob(ctx, j)
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: id, SymlinkPath: "/lib/dead.mkv", TargetPath: "/mnt/torbox/Dead/dead.mkv",
	})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())

	// A heal_failed job still under the limit — should NOT be listed.
	id2, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealFailed, Category: "c", NZBName: "Retrying"})
	j2, _ := st.GetJob(ctx, id2)
	j2.HealCount = 1
	st.UpdateJob(ctx, j2)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/heal_failed", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var items []HealFailedItem
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(items) != 1 || items[0].JobID != id {
		t.Fatalf("expected only the exhausted job, got %+v", items)
	}
	if items[0].HealCount != 3 || items[0].LastHealError != "nzb gone" ||
		len(items[0].BrokenSymlinks) != 1 {
		t.Errorf("bad item: %+v", items[0])
	}
}

func TestHealRetryEndpoint(t *testing.T) {
	srv, st := testServer(t)
	srv.cfg.HealMaxAttempts = 3
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealFailed, Category: "c", NZBName: "n"})
	j, _ := st.GetJob(ctx, id)
	j.HealCount = 3
	j.LastHealError = "boom"
	st.UpdateJob(ctx, j)

	rec := httptest.NewRecorder()
	u := "/health/heal/" + itoaTest(id) + "/retry"
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, u, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := st.GetJob(ctx, id)
	if got.HealCount != 0 || got.LastHealError != "" {
		t.Errorf("retry must reset heal_count and clear the error: %+v", got)
	}
}

func TestHealGiveUpEndpoint(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealFailed, Category: "c", NZBName: "n"})
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: id, SymlinkPath: "/lib/x.mkv", TargetPath: "/mnt/torbox/N/x.mkv",
	})

	rec := httptest.NewRecorder()
	u := "/health/heal/" + itoaTest(id) + "/give_up"
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, u, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateManuallyResolved {
		t.Errorf("give_up must set manually_resolved, got %s", got.State)
	}
	syms, _ := st.ListImportedSymlinks(ctx)
	if len(syms) != 0 {
		t.Errorf("give_up must drop the job's tracked symlinks, got %d", len(syms))
	}
}

// itoaTest renders an int64 for building test URLs.
func itoaTest(n int64) string { return strconv.FormatInt(n, 10) }
```

Add `"strconv"` to the `internal/api/handlers_test.go` import block if it is not already present.

- [ ] **Step 2: Run it — `go test ./internal/api/` — expect FAIL** (undefined `HealFailedItem`).

- [ ] **Step 3: Add the response type**

Append to `internal/api/responses.go`:

```go
// HealFailedItem is one entry in the GET /health/heal_failed list.
type HealFailedItem struct {
	JobID          int64    `json:"job_id"`
	Name           string   `json:"name"`
	BrokenSymlinks []string `json:"broken_symlinks"`
	LastHealError  string   `json:"last_heal_error"`
	HealCount      int64    `json:"heal_count"`
	LastHealedAt   string   `json:"last_healed_at,omitempty"`
}
```

- [ ] **Step 4: Add the handlers and routes**

In `internal/api/handlers.go`, register three routes in `Router()` after the `/health/symlinks` line:

```go
	r.Get("/health/heal_failed", s.handleHealFailed)
	r.Post("/health/heal/{jobID}/retry", s.handleHealRetry)
	r.Post("/health/heal/{jobID}/give_up", s.handleHealGiveUp)
```

Add the three handlers:

```go
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
```

`chi`, `strconv`, `time`, and `job` are already imported in `handlers.go`.

- [ ] **Step 5: Run it — `go test ./internal/api/ -count=1` — expect PASS.**

- [ ] **Step 6: Commit**

```bash
git add internal/api && git commit -m "feat: add heal_failed listing and retry/give_up endpoints"
```

---

## Task 7: Documentation and final verification

**Files:**
- Modify: `.env.example`
- Modify: `README.md`

- [ ] **Step 1: Update `.env.example`**

In `.env.example`, in the heal block (after `SAB2TORBOX_HEAL_BACKOFF_INITIAL`), append:

```
# Optional heal webhook: POST a JSON notification on heal events. Leave the URL
# empty to disable. HEAL_WEBHOOK_EVENTS is a comma-separated subset of
# detected,healing,healed,failed.
SAB2TORBOX_HEAL_WEBHOOK_URL=
SAB2TORBOX_HEAL_WEBHOOK_EVENTS=failed
```

- [ ] **Step 2: Update `README.md`**

In the `## Auto-healing rotated releases` section, after the `HEAL_*` config table, add two `HEAL_WEBHOOK_*` rows describing: `SAB2TORBOX_HEAL_WEBHOOK_URL` (optional, empty = disabled) — URL that receives a JSON POST on heal events; `SAB2TORBOX_HEAL_WEBHOOK_EVENTS` (default `failed`) — comma-separated subset of `detected,healing,healed,failed`.

Then add a `### Monitoring and manual control` subsection at the end of the auto-healing section documenting the four endpoints:

- `GET /health/symlinks` — counts (`tracked`, `broken`, `healing`, `heal_failed`) plus `last_run`/`next_run`.
- `GET /health/heal_failed` — JSON array of jobs the healer has given up on (`heal_count` reached `HEAL_MAX_ATTEMPTS`): each has `job_id`, `name`, `broken_symlinks`, `last_heal_error`, `heal_count`, `last_healed_at`.
- `POST /health/heal/{job_id}/retry` — resets a job's `heal_count` so the healer retries it on the next tick. Use after confirming the NZB should resolve again.
- `POST /health/heal/{job_id}/give_up` — marks the job `manually_resolved` and stops tracking its symlinks; the healer ignores it from then on. Re-acquire the release through Sonarr normally.

Document the webhook JSON shape: an object with `event`, `timestamp`, a `job` object (`id`, `name`, `category`, `heal_count`), and — depending on the event — `symlinks_healed`, `old_torbox_id`, `new_torbox_id`, `error`. Note it is POSTed with `Content-Type: application/json`, best-effort (failures are logged, never retried), and unauthenticated (put a reverse proxy in front if auth is needed).

- [ ] **Step 3: Final verification**

Run: `export PATH="/opt/homebrew/bin:$PATH" && gofmt -s -l . && go vet ./... && golangci-lint run ./... && go test ./... -race -cover -count=1`
Expected: no gofmt output, vet clean, lint clean, all tests PASS, every `internal/` package ≥ 70%.

- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "docs: document the heal webhook and management endpoints"
```

---

## Notes for the implementer

- **Module path** is `github.com/radaiko/sab2torbox`.
- **`git add -A`** only in Task 7; elsewhere stage the listed paths.
- **`go`/`gofmt`/`golangci-lint`** are not on the default PATH — prefix shell commands with `export PATH="/opt/homebrew/bin:$PATH"`.
- The webhook POST is **fire-and-forget** (`go w.postHealWebhook(...)`). That goroutine can outlive a `Run` cancellation; for a best-effort notification that is acceptable. Do not try to track it with a WaitGroup.
- `emitHealEvent` reads `timeNow()` (the package clock) for the timestamp, consistent with the rest of the healer.
- The `manually_resolved` state is naturally ignored by the existing healer: `discoverSymlinks` queries only `imported`/`healing`/`heal_failed`, and `triggerHeals` acts only on `imported`/`heal_failed`. No healer code change is needed for Task 1 beyond the state itself.
