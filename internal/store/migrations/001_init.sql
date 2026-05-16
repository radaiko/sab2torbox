-- +goose Up
CREATE TABLE jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    state TEXT NOT NULL,
    category TEXT NOT NULL,
    nzb_name TEXT NOT NULL,
    nzb_content BLOB,
    nzb_url TEXT,
    nzb_sha256 TEXT,
    torbox_id INTEGER,
    torbox_hash TEXT,
    storage_path TEXT,
    total_bytes INTEGER DEFAULT 0,
    downloaded_bytes INTEGER DEFAULT 0,
    progress_pct INTEGER DEFAULT 0,
    fail_message TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    submitted_at TIMESTAMP,
    completed_at TIMESTAMP
);
CREATE INDEX idx_jobs_state ON jobs(state);
CREATE INDEX idx_jobs_torbox_id ON jobs(torbox_id);
CREATE INDEX idx_jobs_updated ON jobs(updated_at);
CREATE INDEX idx_jobs_sha256 ON jobs(nzb_sha256);

-- +goose Down
DROP TABLE jobs;
