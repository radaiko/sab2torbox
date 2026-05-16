// Package store provides SQLite-backed persistence for sab2torbox jobs.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/radaiko/sab2torbox/internal/job"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store is a SQLite-backed job repository.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path, applies all
// pending migrations, and returns a ready Store. All transactions use
// BEGIN IMMEDIATE to serialize concurrent writers.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate",
		path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite %q: %w", path, err)
	}
	db.SetMaxOpenConns(1) // SQLite single-writer; avoids SQLITE_BUSY churn
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging sqlite: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate applies embedded goose migrations.
func migrate(db *sql.DB) error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("setting goose dialect: %w", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}
	return nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

// Ping verifies the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Exec runs an arbitrary statement. Intended for tests and maintenance.
func (s *Store) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, query, args...)
}

// jobColumns is the canonical column order for SELECT/scan.
const jobColumns = `id, state, category, nzb_name, nzb_content, nzb_url,
	nzb_sha256, torbox_id, torbox_hash, storage_path, total_bytes,
	downloaded_bytes, progress_pct, fail_message, created_at, updated_at,
	submitted_at, completed_at, eta_seconds, heal_count, last_healed_at,
	last_heal_error`

// scanJob reads one job row in jobColumns order.
func scanJob(row interface{ Scan(...any) error }) (*job.Job, error) {
	var j job.Job
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
	return &j, nil
}

// CreateJob inserts j and returns its new ID.
func (s *Store) CreateJob(ctx context.Context, j *job.Job) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (state, category, nzb_name, nzb_content, nzb_url, nzb_sha256)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		j.State, j.Category, j.NZBName, j.NZBContent, nullStr(j.NZBURL), nullStr(j.NZBSHA256))
	if err != nil {
		return 0, fmt.Errorf("inserting job: %w", err)
	}
	return res.LastInsertId()
}

// GetJob loads one job by ID.
func (s *Store) GetJob(ctx context.Context, id int64) (*job.Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id))
	if err != nil {
		return nil, fmt.Errorf("getting job %d: %w", id, err)
	}
	return j, nil
}

// UpdateJob persists every mutable column of j and bumps updated_at.
func (s *Store) UpdateJob(ctx context.Context, j *job.Job) error {
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
	if err != nil {
		return fmt.Errorf("updating job %d: %w", j.ID, err)
	}
	return nil
}

// DeleteJob removes one job row.
func (s *Store) DeleteJob(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id=?`, id); err != nil {
		return fmt.Errorf("deleting job %d: %w", id, err)
	}
	return nil
}

// JobsByState returns all jobs in any of the given states, oldest first.
func (s *Store) JobsByState(ctx context.Context, states ...job.State) ([]*job.Job, error) {
	if len(states) == 0 {
		return nil, nil
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
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE state IN (`+placeholders+`) ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("querying jobs by state: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*job.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// FindBySHA256 returns the most recent job with the given NZB hash and
// category, or nil if none exists.
func (s *Store) FindBySHA256(ctx context.Context, sha, category string) (*job.Job, error) {
	return s.findOne(ctx, `nzb_sha256=? AND category=?`, sha, category)
}

// FindByURL returns the most recent job with the given NZB URL and category,
// or nil if none exists.
func (s *Store) FindByURL(ctx context.Context, url, category string) (*job.Job, error) {
	return s.findOne(ctx, `nzb_url=? AND category=?`, url, category)
}

func (s *Store) findOne(ctx context.Context, where string, args ...any) (*job.Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE `+where+` ORDER BY id DESC LIMIT 1`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("finding job: %w", err)
	}
	return j, nil
}

// ActiveStoragePaths returns the storage_path of every job not in a terminal
// state. The reaper uses it to avoid removing a symlink directory still in use.
func (s *Store) ActiveStoragePaths(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT storage_path FROM jobs WHERE storage_path <> '' AND state NOT IN (?, ?)`,
		job.StateDeleted, job.StateFailed)
	if err != nil {
		return nil, fmt.Errorf("querying active storage paths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ReapImported deletes imported jobs whose updated_at is older than cutoff and
// returns the number removed.
func (s *Store) ReapImported(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM jobs WHERE state=? AND updated_at < ?`, job.StateImported, cutoff)
	if err != nil {
		return 0, fmt.Errorf("reaping imported jobs: %w", err)
	}
	return res.RowsAffected()
}

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

// DeleteImportedSymlinksByJob removes every tracked symlink belonging to a job.
func (s *Store) DeleteImportedSymlinksByJob(ctx context.Context, jobID int64) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM imported_symlinks WHERE job_id=?`, jobID); err != nil {
		return fmt.Errorf("deleting symlinks for job %d: %w", jobID, err)
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

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}
