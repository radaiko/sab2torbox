# Auto-Heal Symlinks — Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When TorBox rotates a release out of storage, sab2torbox automatically resubmits the stored NZB and repairs the now-broken library symlinks in place — no Sonarr re-search, no missing-state.

**Architecture:** A new opt-in `healer` worker. An hourly loop discovers library symlinks into an `imported_symlinks` table, detects broken ones via `stat()`, and triggers a heal by resubmitting the job's stored NZB (state → `healing`). A separate per-`POLL_INTERVAL` reconcile loop finishes `healing` jobs once the new TorBox download is present, atomically rewriting each broken symlink. The healer never blocks waiting on a download.

**Tech Stack:** Go 1.25, existing `internal/worker` loop pattern, `modernc.org/sqlite`, goose migrations.

**Scope:** Phase 1 only. Out of this plan (Phase 2): the webhook, `/health/heal_failed`, `POST /health/heal/{id}/retry`, `POST .../give_up`, the `manually_resolved` state.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/job/job.go` | Add `healing`/`heal_failed` states + transitions; `Job` heal fields; `ImportedSymlink` type |
| `internal/config/config.go` | `HEAL_*` config fields + validation |
| `internal/store/migrations/003_heal.sql` | goose migration: `jobs` heal columns + `imported_symlinks` table |
| `internal/store/store.go` | heal columns in job CRUD; `imported_symlinks` CRUD; count helpers |
| `internal/worker/submitter.go` | Stop nilling `nzb_content` — kept for life as the heal seed |
| `internal/worker/symlink.go` | `atomicReplaceSymlink`, `lcp`, `findBestMatch`, `isVideoFile` |
| `internal/worker/worker.go` | `loopSpec` type; wire healer loops when `HEAL_ENABLED`; `HealRunInfo` |
| `internal/worker/healer.go` | `healOnce` (discover/detect/trigger) and `healReconcileOnce` (finish) |
| `internal/api/handlers.go` | `GET /health/symlinks` |
| `internal/api/responses.go` | `SymlinkHealthResponse` |
| `cmd/sab2torbox/main.go` | Wire the heal reporter into the API server |

---

## Task 1: Heal states and the ImportedSymlink type

**Files:**
- Modify: `internal/job/job.go`
- Modify: `internal/job/job_test.go`

- [ ] **Step 1: Add the failing test**

Append to `internal/job/job_test.go`:

```go
func TestHealTransitions(t *testing.T) {
	cases := []struct {
		from, to State
		want     bool
	}{
		{StateImported, StateHealing, true},
		{StateHealing, StateImported, true},
		{StateHealing, StateHealFailed, true},
		{StateHealFailed, StateHealing, true},
		{StateHealing, StateDeleted, false},
		{StateHealFailed, StateImported, false},
	}
	for _, c := range cases {
		if got := c.from.CanTransitionTo(c.to); got != c.want {
			t.Errorf("%s -> %s: got %v, want %v", c.from, c.to, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run it — `go test ./internal/job/` — expect FAIL** (undefined `StateHealing`).

- [ ] **Step 3: Add the states, transitions, and types**

In `internal/job/job.go`, add to the state constants block:

```go
	StateHealing    State = "healing"     // resubmitted to TorBox, awaiting the new download
	StateHealFailed State = "heal_failed" // resubmission failed; retried with backoff
```

In the `transitions` map, replace the `StateImported` line and add two entries:

```go
	StateImported:    {StateDeleted, StateFailed, StateHealing},
	StateHealing:     {StateImported, StateHealFailed},
	StateHealFailed:  {StateHealing},
```

Add three fields to the `Job` struct, after `ProgressPct`:

```go
	HealCount     int64
	LastHealedAt  *time.Time
	LastHealError string
```

Add the new domain type at the end of the file:

```go
// ImportedSymlink tracks a symlink Sonarr/Radarr moved into its library, so
// the healer can repair it if TorBox rotates the target out of storage.
type ImportedSymlink struct {
	ID           int64
	JobID        int64
	SymlinkPath  string
	TargetPath   string
	DiscoveredAt time.Time
	LastVerified *time.Time
	IsBroken     bool
}
```

- [ ] **Step 4: Run it — `go test ./internal/job/` — expect PASS.**

- [ ] **Step 5: Commit**

```bash
git add internal/job && git commit -m "feat: add heal states and ImportedSymlink type"
```

---

## Task 2: Heal configuration

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: Add the failing test**

Append to `internal/config/config_test.go`:

```go
func TestHealConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAB2TORBOX_TORBOX_API_TOKEN", "t")
	t.Setenv("SAB2TORBOX_SAB_API_KEY", "k")
	t.Setenv("SAB2TORBOX_WEBDAV_MOUNT_ROOT", dir)
	t.Setenv("SAB2TORBOX_SYMLINK_ROOT", dir)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HealEnabled {
		t.Error("HealEnabled must default to false")
	}
	if c.HealInterval != time.Hour {
		t.Errorf("HealInterval default: %v", c.HealInterval)
	}
	if c.HealMaxAttempts != 3 {
		t.Errorf("HealMaxAttempts default: %d", c.HealMaxAttempts)
	}
	if c.HealBackoffInitial != 5*time.Minute {
		t.Errorf("HealBackoffInitial default: %v", c.HealBackoffInitial)
	}
}

func TestHealRequiresLibraryRootsWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAB2TORBOX_TORBOX_API_TOKEN", "t")
	t.Setenv("SAB2TORBOX_SAB_API_KEY", "k")
	t.Setenv("SAB2TORBOX_WEBDAV_MOUNT_ROOT", dir)
	t.Setenv("SAB2TORBOX_SYMLINK_ROOT", dir)
	t.Setenv("SAB2TORBOX_HEAL_ENABLED", "true")

	if _, err := Load(); err == nil {
		t.Fatal("expected error: HEAL_ENABLED without HEAL_LIBRARY_ROOTS")
	}
	t.Setenv("SAB2TORBOX_HEAL_LIBRARY_ROOTS", dir)
	if _, err := Load(); err != nil {
		t.Fatalf("Load with valid heal config: %v", err)
	}
}
```

- [ ] **Step 2: Run it — `go test ./internal/config/` — expect FAIL** (undefined `HealEnabled`).

- [ ] **Step 3: Add the config fields and validation**

In `internal/config/config.go`, add to the `Config` struct after `WebDAVRefreshCooldown`:

```go
	HealEnabled        bool          `envconfig:"HEAL_ENABLED" default:"false"`
	HealInterval       time.Duration `envconfig:"HEAL_INTERVAL" default:"1h"`
	HealLibraryRoots   []string      `envconfig:"HEAL_LIBRARY_ROOTS"`
	HealDryRun         bool          `envconfig:"HEAL_DRY_RUN" default:"false"`
	HealMaxAttempts    int           `envconfig:"HEAL_MAX_ATTEMPTS" default:"3"`
	HealBackoffInitial time.Duration `envconfig:"HEAL_BACKOFF_INITIAL" default:"5m"`
```

In `Load()`, just before `return &c, nil`, add:

```go
	if c.HealEnabled {
		if len(c.HealLibraryRoots) == 0 {
			return nil, fmt.Errorf("HEAL_ENABLED requires HEAL_LIBRARY_ROOTS")
		}
		for _, root := range c.HealLibraryRoots {
			info, err := os.Stat(root)
			if err != nil {
				return nil, fmt.Errorf("heal library root %q: %w", root, err)
			}
			if !info.IsDir() {
				return nil, fmt.Errorf("heal library root %q is not a directory", root)
			}
		}
	}
```

- [ ] **Step 4: Run it — `go test ./internal/config/` — expect PASS.**

- [ ] **Step 5: Commit**

```bash
git add internal/config && git commit -m "feat: add HEAL_* configuration"
```

---

## Task 3: Migration 003 and heal columns in job CRUD

**Files:**
- Create: `internal/store/migrations/003_heal.sql`
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

- [ ] **Step 1: Create the migration**

`internal/store/migrations/003_heal.sql`:

```sql
-- +goose Up
ALTER TABLE jobs ADD COLUMN heal_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN last_healed_at TIMESTAMP;
ALTER TABLE jobs ADD COLUMN last_heal_error TEXT;

CREATE TABLE imported_symlinks (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id        INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    symlink_path  TEXT NOT NULL UNIQUE,
    target_path   TEXT NOT NULL,
    discovered_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_verified TIMESTAMP,
    is_broken     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_imported_symlinks_job ON imported_symlinks(job_id);
CREATE INDEX idx_imported_symlinks_target ON imported_symlinks(target_path);
CREATE INDEX idx_imported_symlinks_broken ON imported_symlinks(is_broken);

-- +goose Down
DROP TABLE imported_symlinks;
ALTER TABLE jobs DROP COLUMN heal_count;
ALTER TABLE jobs DROP COLUMN last_healed_at;
ALTER TABLE jobs DROP COLUMN last_heal_error;
```

- [ ] **Step 2: Add the failing test**

Append to `internal/store/store_test.go`:

```go
func TestHealColumnsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, _ := s.CreateJob(ctx, &job.Job{State: job.StateImported, Category: "c", NZBName: "n"})
	j, _ := s.GetJob(ctx, id)
	now := time.Now().UTC().Truncate(time.Second)
	j.HealCount = 2
	j.LastHealedAt = &now
	j.LastHealError = "boom"
	if err := s.UpdateJob(ctx, j); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	got, _ := s.GetJob(ctx, id)
	if got.HealCount != 2 || got.LastHealError != "boom" || got.LastHealedAt == nil {
		t.Errorf("heal columns not persisted: %+v", got)
	}
}
```

- [ ] **Step 3: Run it — `go test ./internal/store/` — expect FAIL.** The migration adds the DB columns, but `jobColumns`/`scanJob`/`UpdateJob` don't reference them yet, so the round-trip leaves `HealCount` at 0 — the assertion fails.

- [ ] **Step 4: Wire the columns into job CRUD**

In `internal/store/store.go`, change `jobColumns` to append the three columns:

```go
const jobColumns = `id, state, category, nzb_name, nzb_content, nzb_url,
	nzb_sha256, torbox_id, torbox_hash, storage_path, total_bytes,
	downloaded_bytes, progress_pct, fail_message, created_at, updated_at,
	submitted_at, completed_at, eta_seconds, heal_count, last_healed_at,
	last_heal_error`
```

In `scanJob`, add `healError` to the `sql.NullString` group and `healedAt` to the `sql.NullTime` group, extend the `Scan` call, and map them:

```go
	var (
		nzbURL, nzbSHA, hash, storage, failMsg, healError sql.NullString
		torboxID                                          sql.NullInt64
		submitted, completed, healedAt                    sql.NullTime
	)
	err := row.Scan(&j.ID, &j.State, &j.Category, &j.NZBName, &j.NZBContent,
		&nzbURL, &nzbSHA, &torboxID, &hash, &storage, &j.TotalBytes,
		&j.DownloadedBytes, &j.ProgressPct, &failMsg, &j.CreatedAt,
		&j.UpdatedAt, &submitted, &completed, &j.ETASeconds, &j.HealCount,
		&healedAt, &healError)
	if err != nil {
		return nil, err
	}
	j.NZBURL, j.NZBSHA256, j.TorBoxHash = nzbURL.String, nzbSHA.String, hash.String
	j.StoragePath, j.FailMessage, j.TorBoxID = storage.String, failMsg.String, torboxID.Int64
	j.LastHealError = healError.String
	if submitted.Valid {
		j.SubmittedAt = &submitted.Time
	}
	if completed.Valid {
		j.CompletedAt = &completed.Time
	}
	if healedAt.Valid {
		j.LastHealedAt = &healedAt.Time
	}
```

In `UpdateJob`, extend the `SET` clause and args:

```go
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state=?, category=?, nzb_name=?, nzb_content=?,
		 nzb_url=?, nzb_sha256=?, torbox_id=?, torbox_hash=?, storage_path=?,
		 total_bytes=?, downloaded_bytes=?, progress_pct=?, fail_message=?,
		 updated_at=CURRENT_TIMESTAMP, submitted_at=?, completed_at=?,
		 eta_seconds=?, heal_count=?, last_healed_at=?, last_heal_error=?
		 WHERE id=?`,
		j.State, j.Category, j.NZBName, j.NZBContent, nullStr(j.NZBURL),
		nullStr(j.NZBSHA256), nullInt(j.TorBoxID), nullStr(j.TorBoxHash),
		nullStr(j.StoragePath), j.TotalBytes, j.DownloadedBytes, j.ProgressPct,
		nullStr(j.FailMessage), nullTime(j.SubmittedAt), nullTime(j.CompletedAt),
		j.ETASeconds, j.HealCount, nullTime(j.LastHealedAt),
		nullStr(j.LastHealError), j.ID)
```

- [ ] **Step 5: Run it — `go test ./internal/store/` — expect PASS.**

- [ ] **Step 6: Commit**

```bash
git add internal/store && git commit -m "feat: add migration 003 (heal columns + imported_symlinks)"
```

---

## Task 4: imported_symlinks store layer

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

- [ ] **Step 1: Add the failing test**

Append to `internal/store/store_test.go`:

```go
func TestImportedSymlinkCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	jobID, _ := s.CreateJob(ctx, &job.Job{State: job.StateImported, Category: "c", NZBName: "n"})

	sym := &job.ImportedSymlink{JobID: jobID, SymlinkPath: "/lib/a.mkv", TargetPath: "/mnt/torbox/Rel/a.mkv"}
	if err := s.UpsertImportedSymlink(ctx, sym); err != nil {
		t.Fatalf("UpsertImportedSymlink: %v", err)
	}
	// Upsert again with a new target — must update, not duplicate.
	sym.TargetPath = "/mnt/torbox/Rel2/a.mkv"
	if err := s.UpsertImportedSymlink(ctx, sym); err != nil {
		t.Fatalf("UpsertImportedSymlink (update): %v", err)
	}
	list, err := s.ListImportedSymlinks(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListImportedSymlinks: len=%d err=%v", len(list), err)
	}
	if list[0].TargetPath != "/mnt/torbox/Rel2/a.mkv" {
		t.Errorf("target not updated: %q", list[0].TargetPath)
	}

	id := list[0].ID
	if err := s.SetSymlinkVerified(ctx, id, true, time.Now()); err != nil {
		t.Fatalf("SetSymlinkVerified: %v", err)
	}
	if list, _ = s.ListImportedSymlinks(ctx); !list[0].IsBroken {
		t.Error("symlink should be marked broken")
	}
	if err := s.UpdateSymlinkTarget(ctx, id, "/mnt/torbox/Rel3/a.mkv"); err != nil {
		t.Fatalf("UpdateSymlinkTarget: %v", err)
	}
	if list, _ = s.ListImportedSymlinks(ctx); list[0].IsBroken || list[0].TargetPath != "/mnt/torbox/Rel3/a.mkv" {
		t.Errorf("UpdateSymlinkTarget should clear is_broken and set target: %+v", list[0])
	}

	tracked, broken, err := s.SymlinkCounts(ctx)
	if err != nil || tracked != 1 || broken != 0 {
		t.Errorf("SymlinkCounts: tracked=%d broken=%d err=%v", tracked, broken, err)
	}
	if err := s.DeleteImportedSymlink(ctx, id); err != nil {
		t.Fatalf("DeleteImportedSymlink: %v", err)
	}
	if list, _ = s.ListImportedSymlinks(ctx); len(list) != 0 {
		t.Error("symlink row should be deleted")
	}
}

func TestCountJobsByState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.CreateJob(ctx, &job.Job{State: job.StateHealing, Category: "c", NZBName: "a"})
	s.CreateJob(ctx, &job.Job{State: job.StateHealing, Category: "c", NZBName: "b"})
	s.CreateJob(ctx, &job.Job{State: job.StateHealFailed, Category: "c", NZBName: "d"})
	n, err := s.CountJobsByState(ctx, job.StateHealing)
	if err != nil || n != 2 {
		t.Errorf("CountJobsByState healing: n=%d err=%v", n, err)
	}
}
```

- [ ] **Step 2: Run it — `go test ./internal/store/` — expect FAIL** (undefined `UpsertImportedSymlink`).

- [ ] **Step 3: Add the methods**

Append to `internal/store/store.go`:

```go
// importedSymlinkColumns is the canonical column order for scanning.
const importedSymlinkColumns = `id, job_id, symlink_path, target_path,
	discovered_at, last_verified, is_broken`

// UpsertImportedSymlink inserts a tracked symlink, or updates its job_id and
// target if the same symlink_path is recorded again.
func (s *Store) UpsertImportedSymlink(ctx context.Context, sym *job.ImportedSymlink) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO imported_symlinks (job_id, symlink_path, target_path)
		 VALUES (?, ?, ?)
		 ON CONFLICT(symlink_path) DO UPDATE SET
		   job_id = excluded.job_id, target_path = excluded.target_path`,
		sym.JobID, sym.SymlinkPath, sym.TargetPath)
	if err != nil {
		return fmt.Errorf("upserting imported symlink: %w", err)
	}
	return nil
}

// ListImportedSymlinks returns every tracked symlink.
func (s *Store) ListImportedSymlinks(ctx context.Context) ([]*job.ImportedSymlink, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+importedSymlinkColumns+` FROM imported_symlinks ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing imported symlinks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*job.ImportedSymlink
	for rows.Next() {
		var sym job.ImportedSymlink
		var verified sql.NullTime
		var broken int
		if err := rows.Scan(&sym.ID, &sym.JobID, &sym.SymlinkPath, &sym.TargetPath,
			&sym.DiscoveredAt, &verified, &broken); err != nil {
			return nil, err
		}
		if verified.Valid {
			sym.LastVerified = &verified.Time
		}
		sym.IsBroken = broken != 0
		out = append(out, &sym)
	}
	return out, rows.Err()
}

// SetSymlinkVerified records a verification result for one tracked symlink.
func (s *Store) SetSymlinkVerified(ctx context.Context, id int64, broken bool, at time.Time) error {
	b := 0
	if broken {
		b = 1
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE imported_symlinks SET is_broken=?, last_verified=? WHERE id=?`,
		b, at, id); err != nil {
		return fmt.Errorf("setting symlink verified: %w", err)
	}
	return nil
}

// UpdateSymlinkTarget repoints a tracked symlink and clears its broken flag.
func (s *Store) UpdateSymlinkTarget(ctx context.Context, id int64, target string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE imported_symlinks SET target_path=?, is_broken=0 WHERE id=?`,
		target, id); err != nil {
		return fmt.Errorf("updating symlink target: %w", err)
	}
	return nil
}

// DeleteImportedSymlink removes one tracked symlink row.
func (s *Store) DeleteImportedSymlink(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM imported_symlinks WHERE id=?`, id); err != nil {
		return fmt.Errorf("deleting imported symlink: %w", err)
	}
	return nil
}

// SymlinkCounts returns the total tracked symlinks and how many are broken.
func (s *Store) SymlinkCounts(ctx context.Context) (tracked, broken int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(is_broken), 0) FROM imported_symlinks`).
		Scan(&tracked, &broken)
	if err != nil {
		return 0, 0, fmt.Errorf("counting symlinks: %w", err)
	}
	return tracked, broken, nil
}

// CountJobsByState returns how many jobs are in any of the given states.
func (s *Store) CountJobsByState(ctx context.Context, states ...job.State) (int64, error) {
	if len(states) == 0 {
		return 0, nil
	}
	placeholders := ""
	args := make([]any, len(states))
	for i, st := range states {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args[i] = st
	}
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE state IN (`+placeholders+`)`, args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting jobs by state: %w", err)
	}
	return n, nil
}
```

- [ ] **Step 4: Run it — `go test ./internal/store/` — expect PASS.**

- [ ] **Step 5: Commit**

```bash
git add internal/store && git commit -m "feat: add imported_symlinks store layer"
```

---

## Task 5: Keep the NZB blob for life

**Files:**
- Modify: `internal/worker/submitter.go`
- Modify: `internal/worker/worker_test.go`

- [ ] **Step 1: Add the failing test**

Append to `internal/worker/worker_test.go`:

```go
func TestSubmitterKeepsNZBContent(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StatePending, Category: "sonarr", NZBName: "Rel",
		NZBContent: []byte("<nzb/>"),
	})
	if err := w.submitOnce(ctx); err != nil {
		t.Fatalf("submitOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if len(got.NZBContent) == 0 {
		t.Error("nzb_content must be kept after submission (heal seed)")
	}
}
```

- [ ] **Step 2: Run it — `go test ./internal/worker/ -run KeepsNZBContent` — expect FAIL** (content is nilled).

- [ ] **Step 3: Stop nilling the blob**

In `internal/worker/submitter.go`, in `submitJob`, delete the line:

```go
	j.NZBContent = nil // free the blob; TorBox owns it now
```

- [ ] **Step 4: Run it — `go test ./internal/worker/ -run KeepsNZBContent` — expect PASS.**

- [ ] **Step 5: Commit**

```bash
git add internal/worker && git commit -m "feat: keep nzb_content for the job lifetime (heal seed)"
```

---

## Task 6: Symlink heal helpers

**Files:**
- Modify: `internal/worker/symlink.go`
- Modify: `internal/worker/symlink_test.go`

- [ ] **Step 1: Add the failing test**

Append to `internal/worker/symlink_test.go`:

```go
func TestAtomicReplaceSymlink(t *testing.T) {
	dir := t.TempDir()
	oldTarget := filepath.Join(dir, "old.mkv")
	newTarget := filepath.Join(dir, "new.mkv")
	if err := os.WriteFile(oldTarget, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newTarget, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.mkv")
	if err := os.Symlink(oldTarget, link); err != nil {
		t.Fatal(err)
	}
	if err := atomicReplaceSymlink(link, newTarget); err != nil {
		t.Fatalf("atomicReplaceSymlink: %v", err)
	}
	got, _ := os.Readlink(link)
	if got != newTarget {
		t.Errorf("link target: got %q want %q", got, newTarget)
	}
	if b, _ := os.ReadFile(link); string(b) != "new" {
		t.Errorf("content through link: %q", b)
	}
}

func TestLCP(t *testing.T) {
	if lcp("abcdef", "abcxyz") != 3 {
		t.Errorf("lcp = %d, want 3", lcp("abcdef", "abcxyz"))
	}
	if lcp("", "x") != 0 {
		t.Error("lcp with empty string must be 0")
	}
}

func TestFindBestMatch(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"the.rookie.s08e01.GERMAN.mkv", "sample.mkv", "info.nfo"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Case-insensitive prefix match wins over the unrelated sample.
	got, err := findBestMatch(dir, "The.Rookie.S08E01.german.mkv")
	if err != nil {
		t.Fatalf("findBestMatch: %v", err)
	}
	if filepath.Base(got) != "the.rookie.s08e01.GERMAN.mkv" {
		t.Errorf("match: got %q", got)
	}

	// Single video file fallback.
	solo := t.TempDir()
	if err := os.WriteFile(filepath.Join(solo, "totally.different.name.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := findBestMatch(solo, "original.mkv"); err != nil {
		t.Errorf("single-video fallback should match: %v", err)
	}

	// No plausible match -> error.
	empty := t.TempDir()
	if _, err := findBestMatch(empty, "x.mkv"); err == nil {
		t.Error("expected error when nothing matches")
	}
}
```

- [ ] **Step 2: Run it — `go test ./internal/worker/ -run 'AtomicReplace|LCP|FindBestMatch'` — expect FAIL** (undefined `atomicReplaceSymlink`).

- [ ] **Step 3: Add the helpers**

Append to `internal/worker/symlink.go`:

```go
// atomicReplaceSymlink repoints linkPath at newTarget without ever leaving
// linkPath absent: it creates a temp symlink and renames it over linkPath.
// rename(2) is atomic on POSIX, so a process with the file open keeps reading.
func atomicReplaceSymlink(linkPath, newTarget string) error {
	tmp := linkPath + ".heal-tmp"
	_ = os.Remove(tmp) // clear any stale temp link from a crashed run
	if err := os.Symlink(newTarget, tmp); err != nil {
		return fmt.Errorf("creating temp symlink: %w", err)
	}
	if err := os.Rename(tmp, linkPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("atomically replacing symlink: %w", err)
	}
	return nil
}

// lcp returns the length of the longest common prefix of a and b.
func lcp(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// videoExts is the set of extensions treated as the playable video file.
var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m4v": true,
	".ts": true, ".wmv": true, ".mov": true,
}

// isVideoFile reports whether name has a known video extension.
func isVideoFile(name string) bool {
	return videoExts[strings.ToLower(filepath.Ext(name))]
}

// findBestMatch locates the file in dir that most likely corresponds to
// oldBasename, for the case where a re-submitted release names its files
// slightly differently. It never guesses wildly: if nothing plausibly
// matches it returns an error so the caller leaves the old symlink alone.
func findBestMatch(dir, oldBasename string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reading %q: %w", dir, err)
	}
	// 1. Exact match, case-insensitive.
	for _, e := range entries {
		if strings.EqualFold(e.Name(), oldBasename) {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	// 2. Same extension, longest common prefix (case-insensitive).
	oldExt := strings.ToLower(filepath.Ext(oldBasename))
	lowerOld := strings.ToLower(oldBasename)
	var best string
	bestScore := 0
	for _, e := range entries {
		if strings.ToLower(filepath.Ext(e.Name())) != oldExt {
			continue
		}
		if score := lcp(strings.ToLower(e.Name()), lowerOld); score > bestScore {
			best, bestScore = filepath.Join(dir, e.Name()), score
		}
	}
	if best != "" && bestScore > len(oldBasename)/2 {
		return best, nil
	}
	// 3. Exactly one video file in the directory — assume it is the one.
	var videos []string
	for _, e := range entries {
		if !e.IsDir() && isVideoFile(e.Name()) {
			videos = append(videos, e.Name())
		}
	}
	if len(videos) == 1 {
		return filepath.Join(dir, videos[0]), nil
	}
	return "", fmt.Errorf("no match for %q in %q", oldBasename, dir)
}
```

- [ ] **Step 4: Run it — `go test ./internal/worker/ -run 'AtomicReplace|LCP|FindBestMatch'` — expect PASS.**

- [ ] **Step 5: Commit**

```bash
git add internal/worker && git commit -m "feat: add symlink heal helpers (atomic replace, best-match)"
```

---

## Task 7: Worker loop refactor and heal-loop wiring

**Files:**
- Modify: `internal/worker/worker.go`
- Modify: `internal/worker/worker_test.go`

- [ ] **Step 1: Add the failing test**

Append to `internal/worker/worker_test.go`:

```go
func TestHealRunInfoZeroBeforeFirstRun(t *testing.T) {
	w, _, _ := testWorkers(t, &fakeTorBox{})
	last, next := w.HealRunInfo()
	if !last.IsZero() || !next.IsZero() {
		t.Errorf("expected zero times before the first heal run, got %v / %v", last, next)
	}
}
```

- [ ] **Step 2: Run it — `go test ./internal/worker/ -run HealRunInfo` — expect FAIL** (undefined `HealRunInfo`).

- [ ] **Step 3: Refactor `Run` and add the heal accessor**

In `internal/worker/worker.go`, add the import `"sync/atomic"`. Add a field to the `Workers` struct after `webdavBackoffUntil`:

```go
	// healLastRun is the Unix-nanos timestamp of the last healOnce run,
	// read by the /health/symlinks endpoint. Atomic for cross-goroutine reads.
	healLastRun atomic.Int64
```

Replace the `Run` method with a version that uses a named loop type and conditionally adds the heal loops:

```go
// loopSpec is one background loop: a name, a tick interval, and the function
// run each tick.
type loopSpec struct {
	name     string
	interval time.Duration
	fn       func(context.Context) error
}

// Run starts every background loop and blocks until ctx is cancelled.
func (w *Workers) Run(ctx context.Context) {
	loops := []loopSpec{
		{"submitter", w.cfg.PollInterval, w.submitOnce},
		{"poller", w.cfg.PollInterval, w.pollOnce},
		{"deleter", w.cfg.PollInterval, w.deleteOnce},
		{"reaper", 5 * time.Minute, w.reapOnce},
	}
	if w.cfg.HealEnabled {
		loops = append(loops,
			loopSpec{"healer", w.cfg.HealInterval, w.healOnce},
			loopSpec{"heal-reconciler", w.cfg.PollInterval, w.healReconcileOnce},
		)
	}
	var wg sync.WaitGroup
	for _, l := range loops {
		wg.Add(1)
		go func(spec loopSpec) {
			defer wg.Done()
			w.loop(ctx, spec.name, spec.interval, spec.fn)
		}(l)
	}
	wg.Wait()
}

// HealRunInfo returns the last and next scheduled healer run. Both are zero
// before the first run or when healing is disabled.
func (w *Workers) HealRunInfo() (last, next time.Time) {
	n := w.healLastRun.Load()
	if n == 0 {
		return time.Time{}, time.Time{}
	}
	last = time.Unix(0, n)
	return last, last.Add(w.cfg.HealInterval)
}
```

This step references `w.healOnce` and `w.healReconcileOnce`, which Tasks 8–11 create. To keep the build green until then, add a temporary stub file `internal/worker/healer.go`:

```go
// Package worker — healer loop (filled in across the heal tasks).
package worker

import "context"

func (w *Workers) healOnce(ctx context.Context) error          { return nil }
func (w *Workers) healReconcileOnce(ctx context.Context) error { return nil }
```

Tasks 8–11 replace these stubs with the real implementations.

- [ ] **Step 4: Run it — `go test ./internal/worker/ -run HealRunInfo` — expect PASS; `go build ./...` clean.**

- [ ] **Step 5: Commit**

```bash
git add internal/worker && git commit -m "feat: refactor Run with loopSpec; wire heal loops"
```

---

## Task 8: Healer — discover library symlinks

**Files:**
- Modify: `internal/worker/healer.go`
- Modify: `internal/worker/worker_test.go`

- [ ] **Step 1: Add the failing test**

Append to `internal/worker/worker_test.go`:

```go
func TestHealerDiscoversLibrarySymlinks(t *testing.T) {
	w, st, cfg := testWorkers(t, &fakeTorBox{})
	libRoot := t.TempDir()
	cfg.HealLibraryRoots = []string{libRoot}
	ctx := context.Background()

	// A completed job whose release folder is "Rel.A".
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateImported, Category: "sonarr", NZBName: "Rel.A"})
	j, _ := st.GetJob(ctx, id)
	j.StoragePath = filepath.Join(cfg.SymlinkRoot, "sonarr", "Rel.A")
	st.UpdateJob(ctx, j)

	// A library symlink pointing into the WebDAV mount for that release.
	target := filepath.Join(cfg.WebDAVMountRoot, "Rel.A", "ep.mkv")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(target, []byte("v"), 0o644)
	link := filepath.Join(libRoot, "Show", "ep.mkv")
	os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// A symlink pointing somewhere else entirely — must be ignored.
	other := filepath.Join(t.TempDir(), "elsewhere.mkv")
	os.WriteFile(other, []byte("x"), 0o644)
	os.Symlink(other, filepath.Join(libRoot, "Show", "other.mkv"))

	if err := w.discoverSymlinks(ctx); err != nil {
		t.Fatalf("discoverSymlinks: %v", err)
	}
	syms, _ := st.ListImportedSymlinks(ctx)
	if len(syms) != 1 {
		t.Fatalf("expected 1 tracked symlink, got %d", len(syms))
	}
	if syms[0].SymlinkPath != link || syms[0].JobID != id {
		t.Errorf("bad tracked symlink: %+v", syms[0])
	}
}
```

- [ ] **Step 2: Run it — `go test ./internal/worker/ -run Discovers` — expect FAIL** (undefined `discoverSymlinks`).

- [ ] **Step 3: Implement discovery**

Replace `internal/worker/healer.go` with:

```go
package worker

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/radaiko/sab2torbox/internal/job"
)

func (w *Workers) healReconcileOnce(ctx context.Context) error { return nil }

// healOnce runs the periodic heal cycle: discover library symlinks, detect
// broken ones, and trigger heals for affected jobs.
func (w *Workers) healOnce(ctx context.Context) error {
	w.healLastRun.Store(timeNow().UnixNano())
	if err := w.discoverSymlinks(ctx); err != nil {
		w.logger.Error("heal: discovering symlinks", "error", err)
	}
	return nil
}

// discoverSymlinks walks every configured library root and records, in
// imported_symlinks, each symlink that points into the WebDAV mount and whose
// release folder maps to a known job. The hourly re-walk is the source of
// truth — it picks up anything Sonarr renamed or moved.
func (w *Workers) discoverSymlinks(ctx context.Context) error {
	jobs, err := w.store.JobsByState(ctx, job.StateImported, job.StateHealing, job.StateHealFailed)
	if err != nil {
		return err
	}
	byRelease := make(map[string]*job.Job, len(jobs))
	for _, j := range jobs {
		if j.StoragePath != "" {
			byRelease[filepath.Base(j.StoragePath)] = j
		}
	}
	for _, root := range w.cfg.HealLibraryRoots {
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries, keep walking
			}
			if d.Type()&fs.ModeSymlink == 0 {
				return nil
			}
			target, err := os.Readlink(path)
			if err != nil {
				return nil
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			release, ok := releaseUnderRoot(w.cfg.WebDAVMountRoot, target)
			if !ok {
				return nil // points outside the WebDAV mount — not ours
			}
			j, ok := byRelease[release]
			if !ok {
				return nil
			}
			sym := &job.ImportedSymlink{JobID: j.ID, SymlinkPath: path, TargetPath: target}
			if err := w.store.UpsertImportedSymlink(ctx, sym); err != nil {
				w.logger.Warn("heal: recording symlink", "path", path, "error", err)
			}
			return nil
		})
		if walkErr != nil {
			w.logger.Warn("heal: walking library root", "root", root, "error", walkErr)
		}
	}
	return nil
}

// releaseUnderRoot returns the first path component of target beneath
// webdavRoot — the TorBox release folder name — or false if target is not
// under webdavRoot.
func releaseUnderRoot(webdavRoot, target string) (string, bool) {
	rel, err := filepath.Rel(webdavRoot, target)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 0 || parts[0] == "" {
		return "", false
	}
	return parts[0], true
}
```

Also add, to a new file `internal/worker/clock.go`, an overridable clock so heal tests can control time:

```go
package worker

import "time"

// timeNow is the worker package's clock. Tests override it.
var timeNow = time.Now
```

- [ ] **Step 4: Run it — `go test ./internal/worker/ -run Discovers` — expect PASS.**

- [ ] **Step 5: Commit**

```bash
git add internal/worker && git commit -m "feat: healer discovers library symlinks"
```

---

## Task 9: Healer — detect broken symlinks

**Files:**
- Modify: `internal/worker/healer.go`
- Modify: `internal/worker/worker_test.go`

- [ ] **Step 1: Add the failing test**

Append to `internal/worker/worker_test.go`:

```go
func TestHealerDetectsBrokenSymlinks(t *testing.T) {
	w, st, _ := testWorkers(t, &fakeTorBox{})
	ctx := context.Background()
	jobID, _ := st.CreateJob(ctx, &job.Job{State: job.StateImported, Category: "c", NZBName: "n"})

	// A live symlink and a broken one.
	src := t.TempDir()
	good := filepath.Join(src, "good.mkv")
	os.WriteFile(good, []byte("x"), 0o644)
	lib := t.TempDir()
	liveLink := filepath.Join(lib, "live.mkv")
	os.Symlink(good, liveLink)
	brokenLink := filepath.Join(lib, "broken.mkv")
	os.Symlink(filepath.Join(src, "gone.mkv"), brokenLink)
	goneLink := filepath.Join(lib, "gone-entirely.mkv")
	os.Symlink(good, goneLink)

	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{JobID: jobID, SymlinkPath: liveLink, TargetPath: good})
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{JobID: jobID, SymlinkPath: brokenLink, TargetPath: filepath.Join(src, "gone.mkv")})
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{JobID: jobID, SymlinkPath: goneLink, TargetPath: good})
	os.Remove(goneLink) // the symlink itself disappears

	if err := w.detectBrokenSymlinks(ctx); err != nil {
		t.Fatalf("detectBrokenSymlinks: %v", err)
	}
	syms, _ := st.ListImportedSymlinks(ctx)
	got := map[string]bool{}
	for _, s := range syms {
		got[filepath.Base(s.SymlinkPath)] = s.IsBroken
	}
	if len(syms) != 2 {
		t.Fatalf("the vanished symlink row should be deleted; got %d rows", len(syms))
	}
	if got["live.mkv"] {
		t.Error("live symlink wrongly marked broken")
	}
	if !got["broken.mkv"] {
		t.Error("broken symlink not marked broken")
	}
}
```

- [ ] **Step 2: Run it — `go test ./internal/worker/ -run DetectsBroken` — expect FAIL** (undefined `detectBrokenSymlinks`).

- [ ] **Step 3: Implement detection**

In `internal/worker/healer.go`, add the call inside `healOnce` after the discover block (no new imports — `detectBrokenSymlinks` uses only `os` and `timeNow`):

```go
	if err := w.detectBrokenSymlinks(ctx); err != nil {
		w.logger.Error("heal: detecting broken symlinks", "error", err)
	}
```

Add the function:

```go
// detectBrokenSymlinks verifies every tracked symlink. A symlink whose own
// path is gone (Sonarr renamed/moved it) is dropped — the next discovery
// re-records its new location. A symlink whose target is gone is flagged
// broken for the heal pass.
func (w *Workers) detectBrokenSymlinks(ctx context.Context) error {
	syms, err := w.store.ListImportedSymlinks(ctx)
	if err != nil {
		return err
	}
	now := timeNow()
	for _, sym := range syms {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := os.Lstat(sym.SymlinkPath); err != nil {
			if derr := w.store.DeleteImportedSymlink(ctx, sym.ID); derr != nil {
				w.logger.Warn("heal: removing stale symlink row", "id", sym.ID, "error", derr)
			}
			continue
		}
		broken := false
		if _, err := os.Stat(sym.SymlinkPath); err != nil {
			broken = true // Lstat ok but Stat fails -> the target is gone
		}
		if err := w.store.SetSymlinkVerified(ctx, sym.ID, broken, now); err != nil {
			w.logger.Warn("heal: updating symlink state", "id", sym.ID, "error", err)
		}
		if broken {
			w.logger.Warn("heal: broken symlink",
				"path", sym.SymlinkPath, "target", sym.TargetPath)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run it — `go test ./internal/worker/ -run DetectsBroken` — expect PASS.**

- [ ] **Step 5: Commit**

```bash
git add internal/worker && git commit -m "feat: healer detects broken symlinks"
```

---

## Task 10: Healer — trigger heals

**Files:**
- Modify: `internal/worker/healer.go`
- Modify: `internal/worker/worker_test.go`

- [ ] **Step 1: Add the failing test**

Append to `internal/worker/worker_test.go`:

```go
func TestHealerTriggersResubmission(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, cfg := testWorkers(t, fake)
	cfg.HealMaxAttempts = 3
	cfg.HealBackoffInitial = time.Minute
	ctx := context.Background()

	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StateImported, Category: "sonarr", NZBName: "Rel",
		NZBContent: []byte("<nzb/>"),
	})
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: id, SymlinkPath: "/lib/ep.mkv", TargetPath: "/mnt/torbox/Rel/ep.mkv",
	})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())

	if err := w.triggerHeals(ctx); err != nil {
		t.Fatalf("triggerHeals: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateHealing {
		t.Errorf("state: got %s want healing", got.State)
	}
	if got.TorBoxID == 0 {
		t.Error("a new torbox id should be recorded")
	}
	if len(fake.created) != 1 {
		t.Errorf("expected 1 resubmission, got %d", len(fake.created))
	}
}

func TestHealerSkipsExhaustedJobs(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, cfg := testWorkers(t, fake)
	cfg.HealMaxAttempts = 2
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StateHealFailed, Category: "c", NZBName: "n", NZBContent: []byte("x"),
	})
	j, _ := st.GetJob(ctx, id)
	j.HealCount = 2 // already at the limit
	st.UpdateJob(ctx, j)
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{JobID: id, SymlinkPath: "/lib/x.mkv", TargetPath: "/mnt/torbox/N/x.mkv"})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())

	if err := w.triggerHeals(ctx); err != nil {
		t.Fatalf("triggerHeals: %v", err)
	}
	if len(fake.created) != 0 {
		t.Error("a job at HealMaxAttempts must not be resubmitted")
	}
}
```

- [ ] **Step 2: Run it — `go test ./internal/worker/ -run 'TriggersResub|SkipsExhausted'` — expect FAIL** (undefined `triggerHeals`).

- [ ] **Step 3: Implement the trigger**

In `internal/worker/healer.go`, add `"time"` and `"github.com/radaiko/sab2torbox/internal/torbox"` to imports, add the call inside `healOnce` after the detect block:

```go
	if err := w.triggerHeals(ctx); err != nil {
		w.logger.Error("heal: triggering heals", "error", err)
	}
```

Add the functions:

```go
// triggerHeals resubmits the stored NZB for every job that has at least one
// broken symlink and is eligible (not already healing, attempts left, past
// its backoff). The job transitions to `healing`; healReconcileOnce finishes it.
func (w *Workers) triggerHeals(ctx context.Context) error {
	syms, err := w.store.ListImportedSymlinks(ctx)
	if err != nil {
		return err
	}
	brokenByJob := make(map[int64]int)
	for _, sym := range syms {
		if sym.IsBroken {
			brokenByJob[sym.JobID]++
		}
	}
	for jobID, count := range brokenByJob {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		j, err := w.store.GetJob(ctx, jobID)
		if err != nil {
			continue
		}
		if j.State != job.StateImported && j.State != job.StateHealFailed {
			continue // healing already, or deleted — not eligible
		}
		if int(j.HealCount) >= w.cfg.HealMaxAttempts {
			continue // exhausted — manual intervention needed
		}
		if j.LastHealedAt != nil {
			backoff := healBackoff(w.cfg.HealBackoffInitial, j.HealCount)
			if timeNow().Sub(*j.LastHealedAt) < backoff {
				continue // still in backoff
			}
		}
		if w.cfg.HealDryRun {
			w.logger.Info("heal: dry-run, would heal",
				"job_id", j.ID, "broken_symlinks", count)
			continue
		}
		w.startHeal(ctx, j, count)
	}
	return nil
}

// startHeal resubmits a job's stored NZB to TorBox and moves it to `healing`.
func (w *Workers) startHeal(ctx context.Context, j *job.Job, brokenCount int) {
	log := w.logger.With("job_id", j.ID, "nzb_name", j.NZBName)
	if len(j.NZBContent) == 0 && j.NZBURL == "" {
		log.Error("heal: no stored NZB to resubmit")
		w.markHealFailed(ctx, j, "no stored NZB content")
		return
	}
	res, err := w.tb.CreateUsenetDownload(ctx, torbox.CreateRequest{
		NZBContent: j.NZBContent,
		NZBName:    j.NZBName + ".nzb",
		Link:       j.NZBURL,
	})
	if err != nil {
		log.Warn("heal: resubmission failed", "error", err)
		w.markHealFailed(ctx, j, "resubmission failed: "+err.Error())
		return
	}
	j.State = job.StateHealing
	j.TorBoxID = int64(res.UsenetDownloadID)
	j.TorBoxHash = res.Hash
	j.LastHealError = ""
	if err := w.store.UpdateJob(ctx, j); err != nil {
		log.Error("heal: persisting healing state", "error", err)
		return
	}
	log.Info("heal: resubmitted to torbox",
		"torbox_id", j.TorBoxID, "broken_symlinks", brokenCount)
}

// markHealFailed records a failed heal attempt and applies backoff via
// last_healed_at.
func (w *Workers) markHealFailed(ctx context.Context, j *job.Job, msg string) {
	now := timeNow()
	j.State = job.StateHealFailed
	j.HealCount++
	j.LastHealedAt = &now
	j.LastHealError = msg
	if err := w.store.UpdateJob(ctx, j); err != nil {
		w.logger.Error("heal: persisting heal_failed", "job_id", j.ID, "error", err)
	}
}

// healBackoff is exponential: HealBackoffInitial doubled once per prior attempt.
func healBackoff(initial time.Duration, count int64) time.Duration {
	d := initial
	for i := int64(0); i < count; i++ {
		d *= 2
	}
	return d
}
```

- [ ] **Step 4: Run it — `go test ./internal/worker/ -run 'TriggersResub|SkipsExhausted'` — expect PASS.**

- [ ] **Step 5: Commit**

```bash
git add internal/worker && git commit -m "feat: healer triggers NZB resubmission for broken jobs"
```

---

## Task 11: Healer — reconcile healing jobs

**Files:**
- Modify: `internal/worker/healer.go`
- Modify: `internal/worker/worker_test.go`

- [ ] **Step 1: Add the failing test**

Append to `internal/worker/worker_test.go`:

```go
func TestHealReconcileFinishesHeal(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, cfg := testWorkers(t, fake)
	ctx := context.Background()

	// New release folder on the WebDAV mount.
	newRel := "Rel.Healed"
	newDir := filepath.Join(cfg.WebDAVMountRoot, newRel)
	os.MkdirAll(newDir, 0o755)
	os.WriteFile(filepath.Join(newDir, "ep.mkv"), []byte("v"), 0o644)

	// A job in `healing` with a broken library symlink.
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealing, Category: "sonarr", NZBName: newRel})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 700
	j.StoragePath = filepath.Join(cfg.SymlinkRoot, "sonarr", "Rel.Old")
	st.UpdateJob(ctx, j)

	lib := t.TempDir()
	link := filepath.Join(lib, "ep.mkv")
	os.Symlink(filepath.Join(cfg.WebDAVMountRoot, "Rel.Old", "ep.mkv"), link)
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: id, SymlinkPath: link,
		TargetPath: filepath.Join(cfg.WebDAVMountRoot, "Rel.Old", "ep.mkv"),
	})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())

	fake.list = []torbox.UsenetDownload{{
		ID: 700, Name: newRel, Progress: 1,
		DownloadFinished: true, DownloadPresent: true,
	}}
	if err := w.healReconcileOnce(ctx); err != nil {
		t.Fatalf("healReconcileOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateImported {
		t.Fatalf("state: got %s want imported", got.State)
	}
	if got.HealCount != 1 {
		t.Errorf("heal_count: got %d want 1", got.HealCount)
	}
	target, _ := os.Readlink(link)
	want := filepath.Join(newDir, "ep.mkv")
	if target != want {
		t.Errorf("symlink not repointed: got %q want %q", target, want)
	}
}

func TestHealReconcileMarksFailedDownload(t *testing.T) {
	fake := &fakeTorBox{}
	w, st, _ := testWorkers(t, fake)
	ctx := context.Background()
	id, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealing, Category: "c", NZBName: "n"})
	j, _ := st.GetJob(ctx, id)
	j.TorBoxID = 701
	st.UpdateJob(ctx, j)
	fake.list = []torbox.UsenetDownload{{ID: 701, DownloadState: "failed (dead)"}}
	if err := w.healReconcileOnce(ctx); err != nil {
		t.Fatalf("healReconcileOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateHealFailed {
		t.Errorf("state: got %s want heal_failed", got.State)
	}
}
```

- [ ] **Step 2: Run it — `go test ./internal/worker/ -run HealReconcile` — expect FAIL** (the stub `healReconcileOnce` returns nil and does nothing).

- [ ] **Step 3: Implement the reconcile**

In `internal/worker/healer.go`, replace the stub `func (w *Workers) healReconcileOnce(ctx context.Context) error { return nil }` with:

```go
// healReconcileOnce finishes every job in `healing`: once its resubmitted
// download is present on the WebDAV mount, it repoints the broken symlinks
// and returns the job to `imported`. It makes no TorBox call when nothing is
// healing.
func (w *Workers) healReconcileOnce(ctx context.Context) error {
	jobs, err := w.store.JobsByState(ctx, job.StateHealing)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return nil
	}
	list, err := w.tb.ListUsenet(ctx)
	if err != nil {
		return fmt.Errorf("heal: listing torbox usenet: %w", err)
	}
	byID := make(map[int64]torbox.UsenetDownload, len(list))
	for _, d := range list {
		byID[int64(d.ID)] = d
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rec, ok := byID[j.TorBoxID]
		if !ok {
			continue // the resubmitted download is not listed yet
		}
		if rec.Failed() {
			w.logger.Warn("heal: torbox download failed",
				"job_id", j.ID, "download_state", rec.DownloadState)
			w.markHealFailed(ctx, j, "TorBox state: "+rec.DownloadState)
			continue
		}
		if !rec.DownloadFinished || !rec.DownloadPresent {
			continue // still downloading
		}
		w.finishHeal(ctx, j, rec)
	}
	return nil
}

// finishHeal repoints every broken symlink of a job to its new release, then
// returns the job to `imported`.
func (w *Workers) finishHeal(ctx context.Context, j *job.Job, rec torbox.UsenetDownload) {
	log := w.logger.With("job_id", j.ID, "torbox_id", j.TorBoxID)
	newReleaseDir, err := w.resolveStoragePath(ctx, rec.Name)
	if err != nil {
		log.Debug("heal: waiting for webdav path", "error", err)
		return // retry next tick
	}
	syms, err := w.store.ListImportedSymlinks(ctx)
	if err != nil {
		log.Error("heal: loading symlinks", "error", err)
		return
	}
	healed := 0
	for _, sym := range syms {
		if sym.JobID != j.ID || !sym.IsBroken {
			continue
		}
		base := filepath.Base(sym.TargetPath)
		newTarget := filepath.Join(newReleaseDir, base)
		if _, err := os.Stat(newTarget); err != nil {
			match, merr := findBestMatch(newReleaseDir, base)
			if merr != nil {
				log.Warn("heal: no match, leaving symlink broken",
					"symlink", sym.SymlinkPath, "error", merr)
				continue
			}
			newTarget = match
		}
		if err := atomicReplaceSymlink(sym.SymlinkPath, newTarget); err != nil {
			log.Warn("heal: replacing symlink", "symlink", sym.SymlinkPath, "error", err)
			continue
		}
		if err := w.store.UpdateSymlinkTarget(ctx, sym.ID, newTarget); err != nil {
			w.logger.Warn("heal: updating symlink row", "id", sym.ID, "error", err)
		}
		healed++
	}
	now := timeNow()
	j.State = job.StateImported
	// Keep storage_path in symlink-farm form so discovery still matches it
	// by release name and the deleter's guarded cleanup stays correct.
	j.StoragePath = filepath.Join(w.cfg.SymlinkRoot, j.Category, rec.Name)
	j.ProgressPct = 100
	j.HealCount++
	j.LastHealedAt = &now
	j.LastHealError = ""
	if err := w.store.UpdateJob(ctx, j); err != nil {
		log.Error("heal: persisting healed job", "error", err)
		return
	}
	log.Info("heal: completed", "symlinks_healed", healed)
}
```

Add `"fmt"` to the `healer.go` import block.

- [ ] **Step 4: Run it — `go test ./internal/worker/ -run HealReconcile` — expect PASS.**

- [ ] **Step 5: Commit**

```bash
git add internal/worker && git commit -m "feat: healer reconciles healing jobs and repoints symlinks"
```

---

## Task 12: `/health/symlinks` endpoint

**Files:**
- Modify: `internal/api/responses.go`
- Modify: `internal/api/handlers.go`
- Modify: `cmd/sab2torbox/main.go`
- Modify: `internal/api/handlers_test.go`

- [ ] **Step 1: Add the failing test**

Append to `internal/api/handlers_test.go`:

```go
// fakeHealReporter is a canned HealReporter.
type fakeHealReporter struct{ last, next time.Time }

func (f fakeHealReporter) HealRunInfo() (time.Time, time.Time) { return f.last, f.next }

func TestHealthSymlinks(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	jobID, _ := st.CreateJob(ctx, &job.Job{State: job.StateHealing, Category: "c", NZBName: "n"})
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: jobID, SymlinkPath: "/lib/a.mkv", TargetPath: "/mnt/torbox/N/a.mkv",
	})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())
	srv.SetHealReporter(fakeHealReporter{last: time.Now()})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/symlinks", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var resp SymlinkHealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if resp.Tracked != 1 || resp.Broken != 1 || resp.Healing != 1 {
		t.Errorf("counts wrong: %+v", resp)
	}
}
```

- [ ] **Step 2: Run it — `go test ./internal/api/ -run HealthSymlinks` — expect FAIL** (undefined `SymlinkHealthResponse`).

- [ ] **Step 3: Add the response type**

Append to `internal/api/responses.go`:

```go
// SymlinkHealthResponse answers GET /health/symlinks.
type SymlinkHealthResponse struct {
	Tracked    int64  `json:"tracked"`
	Broken     int64  `json:"broken"`
	Healing    int64  `json:"healing"`
	HealFailed int64  `json:"heal_failed"`
	LastRun    string `json:"last_run"`
	NextRun    string `json:"next_run"`
}
```

- [ ] **Step 4: Add the handler and wiring**

In `internal/api/handlers.go`, add `"time"` to the imports. Add the interface and a field:

```go
// HealReporter exposes the healer's schedule for the /health/symlinks endpoint.
type HealReporter interface {
	HealRunInfo() (last, next time.Time)
}
```

Add `healReporter HealReporter` to the `Server` struct (after `health Checker`), and a setter:

```go
// SetHealReporter attaches the healer's status source for /health/symlinks.
func (s *Server) SetHealReporter(r HealReporter) { s.healReporter = r }
```

Register the route in `Router()`, next to `/healthz`:

```go
	r.Get("/health/symlinks", s.handleHealthSymlinks)
```

Add the handler:

```go
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
```

In `cmd/sab2torbox/main.go`, after `srv.SetHealth(...)`, add:

```go
	srv.SetHealReporter(workers)
```

- [ ] **Step 5: Run it — `go test ./internal/api/ -run HealthSymlinks` — expect PASS; `go build ./...` clean.**

- [ ] **Step 6: Commit**

```bash
git add internal/api cmd && git commit -m "feat: add GET /health/symlinks endpoint"
```

---

## Task 13: End-to-end integration test, README, verification

**Files:**
- Create: `internal/worker/heal_integration_test.go`
- Modify: `README.md`
- Modify: `.env.example`

- [ ] **Step 1: Write the end-to-end heal test**

`internal/worker/heal_integration_test.go`:

```go
package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/radaiko/sab2torbox/internal/config"
	"github.com/radaiko/sab2torbox/internal/job"
	"github.com/radaiko/sab2torbox/internal/store"
	"github.com/radaiko/sab2torbox/internal/torbox"
)

// TestEndToEndRotationHeal simulates: a release is imported, TorBox rotates it
// out (target deleted), the healer resubmits, and the library symlink is
// repointed at the new release folder.
func TestEndToEndRotationHeal(t *testing.T) {
	mountRoot := t.TempDir()
	symlinkRoot := t.TempDir()
	libRoot := t.TempDir()
	newRelease := "Rel.Rotated"

	// Mock TorBox: create returns a new id; mylist reports it finished.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/api/usenet/createusenetdownload":
			w.Write([]byte(`{"success":true,"data":{"usenetdownload_id":9100,"hash":"h"}}`))
		case "/v1/api/usenet/mylist":
			resp := map[string]any{"success": true, "data": []any{map[string]any{
				"id": 9100, "name": newRelease, "progress": 1,
				"download_finished": true, "download_present": true,
			}}}
			json.NewEncoder(w).Encode(resp)
		}
	}))
	defer srv.Close()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "heal.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		WebDAVMountRoot: mountRoot, SymlinkRoot: symlinkRoot,
		PollInterval: time.Millisecond, HealEnabled: true,
		HealLibraryRoots: []string{libRoot}, HealInterval: time.Millisecond,
		HealMaxAttempts: 3, HealBackoffInitial: time.Millisecond,
	}
	tb := torbox.NewWithBaseURL("tok", srv.URL+"/v1/api")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := New(st, tb, cfg, logger)
	ctx := context.Background()

	// An imported job; storage_path in symlink-farm form names the old release.
	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StateImported, Category: "sonarr", NZBName: newRelease,
		NZBContent: []byte("<nzb/>"),
	})
	j, _ := st.GetJob(ctx, id)
	j.StoragePath = filepath.Join(symlinkRoot, "sonarr", newRelease)
	st.UpdateJob(ctx, j)

	// The library symlink Sonarr left behind, pointing at the (soon gone) target.
	oldTarget := filepath.Join(mountRoot, newRelease, "ep.mkv")
	os.MkdirAll(filepath.Dir(oldTarget), 0o755)
	os.WriteFile(oldTarget, []byte("v"), 0o644)
	link := filepath.Join(libRoot, "Show", "ep.mkv")
	os.MkdirAll(filepath.Dir(link), 0o755)
	os.Symlink(oldTarget, link)

	// Heal cycle 1: discover the symlink (target still present, not broken).
	if err := w.healOnce(ctx); err != nil {
		t.Fatalf("healOnce 1: %v", err)
	}

	// TorBox rotates the release out: the target disappears.
	os.RemoveAll(filepath.Join(mountRoot, newRelease))

	// Heal cycle 2: detect broken + resubmit -> job goes `healing`.
	if err := w.healOnce(ctx); err != nil {
		t.Fatalf("healOnce 2: %v", err)
	}
	if got, _ := st.GetJob(ctx, id); got.State != job.StateHealing {
		t.Fatalf("after resubmit: state %s, want healing", got.State)
	}

	// The new release folder appears on the WebDAV mount.
	newDir := filepath.Join(mountRoot, newRelease)
	os.MkdirAll(newDir, 0o755)
	os.WriteFile(filepath.Join(newDir, "ep.mkv"), []byte("v2"), 0o644)

	// Reconcile: finish the heal.
	if err := w.healReconcileOnce(ctx); err != nil {
		t.Fatalf("healReconcileOnce: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateImported || got.HealCount != 1 {
		t.Fatalf("after heal: state=%s heal_count=%d", got.State, got.HealCount)
	}
	target, _ := os.Readlink(link)
	if target != filepath.Join(newDir, "ep.mkv") {
		t.Errorf("symlink not healed: %q", target)
	}
	if b, _ := os.ReadFile(link); string(b) != "v2" {
		t.Errorf("healed symlink reads stale content: %q", b)
	}
}

// TestEndToEndHealResubmitFailure verifies a failed resubmission backs off.
func TestEndToEndHealResubmitFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"success":false,"detail":"nope"}`))
	}))
	defer srv.Close()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "heal2.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		WebDAVMountRoot: t.TempDir(), SymlinkRoot: t.TempDir(),
		PollInterval: time.Millisecond, HealEnabled: true,
		HealLibraryRoots: []string{t.TempDir()}, HealInterval: time.Millisecond,
		HealMaxAttempts: 3, HealBackoffInitial: time.Hour,
	}
	tb := torbox.NewWithBaseURL("tok", srv.URL+"/v1/api")
	w := New(st, tb, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	id, _ := st.CreateJob(ctx, &job.Job{
		State: job.StateImported, Category: "c", NZBName: "n", NZBContent: []byte("x"),
	})
	st.UpsertImportedSymlink(ctx, &job.ImportedSymlink{
		JobID: id, SymlinkPath: "/lib/x.mkv", TargetPath: "/mnt/torbox/n/x.mkv",
	})
	syms, _ := st.ListImportedSymlinks(ctx)
	st.SetSymlinkVerified(ctx, syms[0].ID, true, time.Now())

	if err := w.triggerHeals(ctx); err != nil {
		t.Fatalf("triggerHeals: %v", err)
	}
	got, _ := st.GetJob(ctx, id)
	if got.State != job.StateHealFailed {
		t.Errorf("state: got %s want heal_failed", got.State)
	}
	if got.HealCount != 1 || got.LastHealError == "" {
		t.Errorf("failed heal not recorded: count=%d err=%q", got.HealCount, got.LastHealError)
	}
}
```

- [ ] **Step 2: Run it — `go test ./internal/worker/ -run EndToEnd -v` — expect PASS** (both heal tests and the existing submit/poll one).

- [ ] **Step 3: Run the full suite with coverage**

Run: `go test ./internal/... -race -cover`
Expected: PASS; every `internal/` package ≥ 70%. If `internal/worker` dipped below, add a focused test for the uncovered branch (e.g. `releaseUnderRoot` rejecting an outside path, `healBackoff` growth).

- [ ] **Step 4: Update `.env.example`**

Append to `.env.example`:

```
# Auto-heal symlinks broken by TorBox rotating releases out of storage.
# Disabled by default. When enabled, HEAL_LIBRARY_ROOTS is required.
SAB2TORBOX_HEAL_ENABLED=false
SAB2TORBOX_HEAL_INTERVAL=1h
SAB2TORBOX_HEAL_LIBRARY_ROOTS=/mnt/smedia/tv,/mnt/smedia/movies
SAB2TORBOX_HEAL_DRY_RUN=false
SAB2TORBOX_HEAL_MAX_ATTEMPTS=3
SAB2TORBOX_HEAL_BACKOFF_INITIAL=5m
```

- [ ] **Step 5: Update `README.md`**

Add a section titled `## Auto-healing rotated releases` immediately before `## Troubleshooting`. It must explain:

- TorBox rotates releases out of storage after roughly 30 days; the library
  symlinks then dangle and Plex shows "Unplayable".
- With `SAB2TORBOX_HEAL_ENABLED=true` and `SAB2TORBOX_HEAL_LIBRARY_ROOTS` set
  to the comma-separated Sonarr/Radarr library roots, the healer (hourly) walks
  those roots, detects broken symlinks, resubmits the original stored NZB to
  TorBox (which usually hits TorBox's cache and finishes in seconds), and
  atomically repoints the symlinks at the new release folder. Sonarr never
  sees a missing state.
- The full `HEAL_*` variable table (the six variables from `.env.example`,
  with the descriptions from the design spec).
- That `nzb_content` is now kept for each job's lifetime as the heal seed —
  storage cost is tens of KB per job (~30 MB for 10,000 jobs).
- `GET /health/symlinks` returns `{tracked, broken, healing, heal_failed,
  last_run, next_run}`.

Add three entries under `## Troubleshooting`:

```
**Heal is not running.**
Check `SAB2TORBOX_HEAL_ENABLED=true` and that `SAB2TORBOX_HEAL_LIBRARY_ROOTS`
lists every Sonarr/Radarr library root. The service fails to start if
HEAL_ENABLED is set without valid HEAL_LIBRARY_ROOTS.

**Heal keeps failing for one release.**
After `HEAL_MAX_ATTEMPTS` the healer gives up on that job. The NZB may have
aged off Usenet so TorBox can no longer fetch it — delete the item in Sonarr
and let it re-search for a different release.

**A broken symlink isn't being healed.**
The healer only tracks symlinks whose target is under `WEBDAV_MOUNT_ROOT` and
that sit inside a `HEAL_LIBRARY_ROOTS` path. Confirm those roots cover every
library folder; the hourly re-walk then picks the symlink up.
```

- [ ] **Step 6: Final verification**

Run: `gofmt -s -l . && go vet ./... && golangci-lint run ./... && go test ./... -race -cover`
Expected: no gofmt output, vet clean, lint clean, all tests PASS.

- [ ] **Step 7: Commit**

```bash
git add -A && git commit -m "test: end-to-end heal integration test; docs for auto-healing"
```

---

## Notes for the implementer

- **Module path** is `github.com/radaiko/sab2torbox`.
- **`git add -A`**, never `git commit -a` — several tasks add new files (`healer.go`, `clock.go`, `heal_integration_test.go`, `003_heal.sql`).
- **`timeNow`** (in `clock.go`) is the package clock; heal logic uses it so future tests can simulate backoff windows. Production code path is unaffected (`timeNow = time.Now`).
- **`storage_path` after a heal** is deliberately written in symlink-farm form (`<SymlinkRoot>/<category>/<release>`), even though no farm is rebuilt — `discoverSymlinks` matches jobs by `filepath.Base(storage_path)`, and the deleter's guarded `removeSymlinkDir` treats a missing directory as a no-op.
- **Heal reconcile cadence:** `healOnce` runs at `HEAL_INTERVAL` (1h — rotation is a slow problem); `healReconcileOnce` runs at `POLL_INTERVAL` (1m) so a resubmitted, cache-hit download is picked up within ~a minute. `healReconcileOnce` makes no TorBox call when nothing is `healing`.
- **CI Go version** is 1.25; `go.mod` already targets it.
