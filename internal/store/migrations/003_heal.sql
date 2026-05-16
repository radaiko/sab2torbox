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
