-- +goose Up
ALTER TABLE jobs ADD COLUMN eta_seconds INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE jobs DROP COLUMN eta_seconds;
