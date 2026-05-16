# sab2torbox — Design

**Date:** 2026-05-16
**Status:** Approved for planning

## What this is and why

A Go HTTP service that impersonates SABnzbd to Sonarr/Radarr. Instead of
downloading NZBs to local disk, it submits them to TorBox's Usenet API and
surfaces the completed files through TorBox's WebDAV mount. Goal: **zero local
storage** — bytes only ever physically exist on TorBox's servers (cached via
rclone's VFS). Sonarr "imports" by moving files within the same rclone-mounted
filesystem, so the move is a rename, not a byte copy.

TorBoxarr does roughly the same job but copies files to local disk in its
`local_downloading` stage. sab2torbox deliberately does not — the WebDAV mount
is the only place the files appear to Sonarr.

## Tech stack (locked)

- **Language:** Go 1.23+
- **HTTP:** `net/http` + `github.com/go-chi/chi/v5`
- **Storage:** SQLite via `modernc.org/sqlite` (pure Go, no CGO)
- **Migrations:** `github.com/pressly/goose/v3`, embedded in the binary
- **Config:** env vars via `github.com/kelseyhightower/envconfig`
- **Logging:** `log/slog` (stdlib)
- **TorBox HTTP client:** `net/http`, 30s timeout, exponential backoff via
  `github.com/cenkalti/backoff/v4`
- **Container:** multi-stage Dockerfile, runtime `gcr.io/distroless/static-debian12:nonroot`
- **License:** MIT

No other dependencies without explicit need. No ORM, no DI framework.

## Project layout

```
sab2torbox/
├── cmd/sab2torbox/main.go            # entry point + healthcheck subcommand
├── internal/
│   ├── api/         handlers.go handlers_test.go responses.go
│   ├── config/      config.go
│   ├── store/       store.go store_test.go migrations/001_init.sql
│   ├── torbox/      client.go client_test.go types.go
│   ├── worker/      poller.go poller_test.go
│   └── job/         job.go
├── deploy/          Dockerfile docker-compose.yml
├── .github/workflows/ ci.yml release.yml
├── .env.example  go.mod  README.md  LICENSE
```

## Architecture

Three layers that communicate **only through SQLite**:

1. **API layer** (`internal/api`) — SABnzbd-compatible HTTP handlers. Each
   request is a single DB query + JSON marshal. **Never calls the TorBox API.**
   Sonarr polls `queue`/`history` frequently, so handlers must stay fast.
2. **Workers** (`internal/worker`) — three goroutines, the *only* code that
   touches the TorBox API:
   - **Submitter** — picks up `pending` jobs, submits to TorBox, → `queued`.
   - **Poller** — every `POLL_INTERVAL`, one `GET /usenet/mylist` call,
     filtered locally; updates progress, → `downloading`/`completed`.
   - **Reaper** — every 5 min, deletes `imported` job rows older than 24h
     (safety net for orphans Sonarr never deleted).
3. **Store** (`internal/store`) — SQLite + goose migrations run on startup.
   `BEGIN IMMEDIATE` transactions serialize submit/poll/delete on a job row.

All workers are context-cancellable. Graceful shutdown on SIGTERM/SIGINT.

### State machine (`internal/job`)

```
pending → submitting → queued → downloading → completed → imported → deleted
                                                       \→ failed
```

- `pending` — NZB received from Sonarr, not yet sent to TorBox.
- `submitting` — submit in progress.
- `queued` / `downloading` — derived from TorBox `download_state` +
  `download_finished`.
- `completed` — `download_finished == true` AND `download_present == true`;
  storage path resolved and verified on disk.
- `imported` — Sonarr has read the `history` entry (assumed importing/done).
- `deleted` — removed from TorBox at Sonarr's request.
- `failed` — terminal: submit failed after retries, TorBox failure state, etc.
  Reachable from any non-terminal state.

## TorBox API (verified against `torbox-sdk-py`, 2026-05-16)

Base: `https://api.torbox.app/v1/api`. Header `Authorization: Bearer <token>`
on every request. Every response is the envelope
`{ "success": bool, "error": any, "detail": string, "data": ... }`.

### Endpoints used

- `POST /usenet/createusenetdownload` — multipart form. Fields: `file` (NZB
  bytes) **or** `link` (URL to NZB); optional `name`, `password`,
  `post_processing` (`-1` default … `3` repair+unpack+delete), `as_queued`.
  `data` → `{ usenetdownload_id, hash, auth_id }`.
- `GET /usenet/mylist?bypass_cache=true` — list all; `&id=<id>` for one.
  `offset`/`limit` supported (default limit 1000).
- `POST /usenet/controlusenetdownload` — JSON body
  `{ "usenet_id": <int>, "operation": "delete" }` (also `pause`, `resume`,
  `reannounce`).
- Healthcheck probe: `GET /usenet/mylist?limit=1` — cheap reachability +
  token-validity check (TorBox has no dedicated validate-token endpoint).

### `mylist` record fields (verified)

`id, hash, name, size, download_state, download_finished, download_present,
progress, download_speed, eta, active, created_at, updated_at, expires_at,
files[]`. Each `files[]` entry: `id, name, short_name, size, mimetype, md5,
s3_path`.

### Quirks to bake in

1. **ID type instability.** The SDK types `usenetdownload_id` as *string* and
   `mylist.id` as *float*; the live API serializes them as JSON numbers. The
   TorBox client MUST parse IDs defensively — accept string-or-number and
   normalize to `int64`. This is the single most likely silent bug.
2. **`progress` is a float 0.0–1.0**, not 0–100. Compute
   `progress_pct = round(progress * 100)`.
3. **`files[].name` vs `short_name`.** `name` may carry a path prefix;
   `short_name` is the bare filename. During implementation, verify which
   matches the WebDAV layout; document the finding in the README.
4. The auto-generated SDK doc only lists `file`/`link` for the create request;
   `name`/`password`/`post_processing`/`as_queued` are real optional fields
   confirmed by the docs site. Send `file`/`link` + optional `name`.

Any further quirks found during implementation go in the README.

### TorBox → job-state mapping

- `download_state` in {queued, queued-like} → `queued`.
- `download_state` indicates active transfer, not finished → `downloading`.
- `download_finished && download_present` → `completed`.
- TorBox failure/error state → `failed` with `fail_message` populated.

## SABnzbd API surface

Implement only what Sonarr/Radarr call. All requests are `GET`/`POST` to
`/sabnzbd/api` **and** `/api` (compatibility alias), multiplexed by a `mode`
query param. The `apikey` query param is checked on every request against
`SAB2TORBOX_SAB_API_KEY`; mismatch → SAB-shaped auth error.

| `mode` | Purpose |
|---|---|
| `version` | `{"version": "4.3.0"}` |
| `get_config` | categories + `complete_dir`/`download_dir` (see below) |
| `fullstatus` | minimal stub `{"status": {"paused": false}}` |
| `addurl` | submit NZB by URL → `{"status": true, "nzo_ids": ["sab2tb_<id>"]}` |
| `addfile` | submit NZB by multipart file upload; same response shape |
| `queue` | non-completed jobs as `slots[]` |
| `history` | completed/failed jobs as `slots[]` |
| `queue` + `name=delete` | `value=<nzo_id>` (comma-sep); `del_files=1` also deletes from TorBox |
| `history` + `name=delete` | same shape |

`nzo_id` format: `sab2tb_<job_id>` where `<job_id>` is the SQLite primary key.

### Response shapes

`get_config`:
```json
{ "config": { "misc": { "complete_dir": "/mnt/torbox/usenet",
  "download_dir": "/mnt/torbox/usenet" },
  "categories": [ {"name":"*","dir":""}, {"name":"sonarr","dir":"sonarr"},
  {"name":"radarr","dir":"radarr"}, {"name":"sonarr-anime","dir":"sonarr-anime"} ] } }
```

`queue` slot: `nzo_id, filename, cat, status, mb, mbleft, percentage, timeleft`.

`history` slot: `nzo_id, name, category, status, storage, bytes, fail_message`.

**Critical:** `history.storage` is the path Sonarr reads from — the host's
rclone-mounted WebDAV path. If Sonarr's library root is on the same mount, its
import is a rename. This is the architectural lynchpin.

## Path resolution

When a download completes:

1. Read the TorBox record: `name` (release name) + `files[]`.
2. Expected layout:
   `<WEBDAV_MOUNT_ROOT>/<WEBDAV_USENET_SUBPATH>/<name>/`.
3. Verify the path exists on disk — WebDAV listing lags TorBox's "complete"
   flag by a few seconds. Poll the filesystem: 1s interval, 30s timeout.
4. Store the resolved, verified path in `jobs.storage_path`.
5. Return it as `history.storage`.

`storage_path` is category-agnostic (TorBox lays all Usenet downloads under
`/usenet/<name>/`). Category is still reported back to Sonarr correctly;
Sonarr's import moves files into its own category-specific library folder.

## Configuration (env vars)

| Variable | Required | Default | Description |
|---|---|---|---|
| `SAB2TORBOX_TORBOX_API_TOKEN` | yes | — | TorBox API token |
| `SAB2TORBOX_SAB_API_KEY` | yes | — | API key Sonarr/Radarr authenticates with |
| `SAB2TORBOX_WEBDAV_MOUNT_ROOT` | yes | — | rclone WebDAV mount path, e.g. `/mnt/torbox` |
| `SAB2TORBOX_WEBDAV_USENET_SUBPATH` | no | `usenet` | subpath under the mount root |
| `SAB2TORBOX_LISTEN_ADDR` | no | `:8080` | HTTP bind address |
| `SAB2TORBOX_DATABASE_PATH` | no | `/config/sab2torbox.db` | SQLite path |
| `SAB2TORBOX_POLL_INTERVAL` | no | `10s` | TorBox poll cadence |
| `SAB2TORBOX_LOG_LEVEL` | no | `info` | debug/info/warn/error |
| `SAB2TORBOX_CATEGORIES` | no | `sonarr,radarr,sonarr-anime` | allowed categories |

Validated on startup; fail fast on missing required values or an unreachable
WebDAV mount path.

## Database schema (`migrations/001_init.sql`)

```sql
CREATE TABLE jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    state TEXT NOT NULL,
    category TEXT NOT NULL,
    nzb_name TEXT NOT NULL,
    nzb_content BLOB,
    nzb_url TEXT,
    nzb_sha256 TEXT,                  -- hex SHA256 of NZB content, for addfile dedup
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
```

`nzb_sha256` is the one addition to the original spec schema — it makes
idempotency a DB lookup instead of a content scan.

## Critical behaviors

1. **Idempotent submission.** Re-submitted NZBs (Sonarr retries) are detected
   and the existing `nzo_id` is returned. `addfile`: by `SHA256(NZB content)`;
   `addurl`: by `nzb_url`. Both also scoped by `category`.
2. **WebDAV verification with retry** — as in Path Resolution above.
3. **Same-filesystem move.** README must warn three times: the Sonarr/Radarr
   library root MUST be on the rclone WebDAV mount, or imports become slow byte
   copies and storage is no longer zero.
4. **Fast handlers.** No TorBox calls from request handlers — read local SQLite
   only.
5. **Category vs. path.** `storage_path` is category-agnostic; category is
   reported correctly; Sonarr handles the category-specific move.
6. **Deletion.** SAB delete with `del_files=1` → TorBox
   `controlusenetdownload` `operation=delete`. Without it → drop the local job
   row only, leave the file on TorBox.
7. **Failure surface.** Permanent TorBox failure → job `failed`, `history`
   entry with `status=Failed` and a populated `fail_message`; Sonarr marks the
   release failed and tries the next.

## Healthcheck

`GET /healthz` returns 200 when the DB is reachable and the TorBox token
validates. Token validation uses `GET /usenet/mylist?limit=1`, **cached for 5
minutes**, so the healthcheck doesn't hammer TorBox.

The binary also has a `healthcheck` subcommand that HTTP-GETs its own
`/healthz` — needed because the distroless image has no shell or wget for the
Docker `HEALTHCHECK`.

## Container & CI

- **Dockerfile:** multi-stage. Build on `golang:1.23-alpine` (static binary,
  `CGO_ENABLED=0`); runtime `gcr.io/distroless/static-debian12:nonroot`.
  Expose 8080. `HEALTHCHECK` runs the `healthcheck` subcommand.
- **`ci.yml`:** on push/PR — `gofmt -s` check, `golangci-lint run`,
  `go test ./...`, and a Docker build (no push).
- **`release.yml`:** builds and pushes multi-arch (`linux/amd64`,
  `linux/arm64`) images to `ghcr.io/radaiko/sab2torbox` — `:latest` on push to
  `main`, `:vX.Y.Z` on git tags. Uses `docker/build-push-action` + buildx.
- `docker-compose.yml` per the original spec (mounts `/mnt/torbox` `rshared`,
  `./config:/config`, healthcheck via the `healthcheck` subcommand).

## Testing

- Unit tests: state-machine transitions; SAB response shapes (struct equality
  / golden).
- TorBox client behind an interface; fake implementation for poller tests.
- Integration test against a mock TorBox `httptest.Server`: submit → poll →
  complete → delete.
- Coverage target: 70% on `internal/`. CI runs `go test ./...`.

## Coding standards

- `gofmt -s` + `golangci-lint run` clean.
- All exported types/functions documented.
- No `panic()` outside `main` startup.
- Errors wrapped: `fmt.Errorf("doing X: %w", err)`.
- Structured logging: every line carries `request_id` (when applicable),
  `job_id`, `torbox_id` where relevant.
- No global state except logger and config (passed via struct fields/context).
- Context propagated through all async operations.

## Out of scope

Torrent support (use Decypharr); web UI (TorBox dashboard exists); multi-tenant;
NNTP fallback; repair/health-checking of existing TorBox content; Stremio addon.

## README requirements

what-and-why paragraph; ASCII flow diagram (Sonarr → sab2torbox → TorBox →
WebDAV mount → Sonarr library); prominent same-filesystem warning; quickstart
(docker-compose + Sonarr download-client config steps); reference rclone mount
config for TorBox WebDAV (VFS cache mode/sizes suited to media playback);
troubleshooting (files-don't-appear / slow-import / duplicate-submissions);
honest compare/contrast with TorBoxarr and Decypharr; roadmap & non-goals.

## Definition of done

`docker compose up -d` brings the service online; Sonarr configures it as a
SABnzbd client and submits an NZB; the job appears in `queue`, progresses,
completes; Sonarr imports via the WebDAV mount with no measurable host
primary-disk I/O; `del_files=1` deletion removes the item from TorBox; tests
pass; CI green; README complete.
