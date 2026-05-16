# sab2torbox

A small Go service that **pretends to be SABnzbd** to Sonarr/Radarr but submits
NZBs to [TorBox](https://torbox.app)'s Usenet API instead of downloading them
locally. The real bytes only ever exist on TorBox's servers, reached through a
read-only rclone WebDAV mount. On completion sab2torbox publishes a **symlink
farm** — one symlink per file — and Sonarr imports by moving the tiny *symlink*
into its library. The result is a **zero-local-storage** Usenet bridge.

## How it works

```
   Sonarr/Radarr                sab2torbox                 TorBox
   ┌───────────┐  addfile/    ┌────────────┐  create     ┌─────────┐
   │  SABnzbd  ├─ addurl ────▶│  API layer │── usenet ───▶│ Usenet  │
   │  client   │◀─ queue/ ────┤  (SQLite)  │  download    │ backend │
   └─────┬─────┘  history     └─────▲──────┘              └────┬────┘
         │                          │ poll mylist              │
         │ import (symlink rename)  └──────────────────────────┘
         ▼                                                      │
   ┌─ /mnt/smedia (one local filesystem) ─┐   ┌─ rclone WebDAV ──┘
   │  _incoming/<cat>/<release>/  ─symlink─┼──▶│ /mnt/torbox/<release>/
   │  tv/Show/...   ◀── library            │   │   show.s01e01.mkv
   └───────────────────────────────────────┘   └─ (real bytes, read-only)
```

1. Sonarr submits an NZB to the SABnzbd-compatible API.
2. sab2torbox stores it and a background worker submits it to TorBox.
3. A poller watches TorBox's `mylist` until the download finishes.
4. On completion sab2torbox finds the release folder on the WebDAV mount and
   builds a symlink farm at `<SYMLINK_ROOT>/<category>/<release>/` — one
   symlink per file, pointing at the real file on the mount — then reports
   that directory as the SAB `history` `storage` path.
5. Sonarr moves the symlink into its library. Since `SYMLINK_ROOT` and the
   library share a filesystem, the move is an instant rename.
6. When Sonarr deletes the download (with `del_files`), sab2torbox removes it
   from TorBox and deletes the symlink directory.

Why symlinks? TorBox's WebDAV is **read-only** — Sonarr cannot move or rename
anything on it. The symlink farm lives on a normal writable filesystem, so the
import works while the media bytes still never leave TorBox.

## ⚠️ Critical requirement: imports must be a rename

> `SAB2TORBOX_SYMLINK_ROOT` and the Sonarr/Radarr library **must be on the same
> local filesystem** (e.g. both under `/mnt/smedia`). Then Sonarr's import is a
> symlink rename. If they are on different filesystems the move dereferences
> the symlink into a full byte copy from TorBox — slow, and storage is no
> longer zero.

## The symlink farm

On completion sab2torbox creates, under `SYMLINK_ROOT`:

```
  TorBox WebDAV mount         Symlink farm                  Library
  /mnt/torbox/<release>/  ◀──  /mnt/smedia/_incoming/   ──▶  /mnt/smedia/tv/
    show.s01e01.mkv       ◀──    sonarr/<release>/           Show/s01e01.mkv
    (real bytes)                 show.s01e01.mkv  ──────────┘ (symlink, moved)
```

Requirements:
- `SYMLINK_ROOT` and the library on the same local filesystem (e.g. both under
  `/mnt/smedia`) so the symlink move is a rename.
- Every container that *reads* the media — Sonarr for analysis, Plex/Jellyfin —
  must mount the WebDAV path (`/mnt/torbox`) at the **same absolute path**, or
  the symlinks dangle.
- sab2torbox cleans the farm itself: the deleter removes a release directory
  when Sonarr deletes the download; the reaper sweeps out empty (post-import)
  and orphaned directories. It never removes `<SYMLINK_ROOT>/<category>/`.

## ⚠️ WebDAV layout: folder mode required

> sab2torbox expects TorBox's WebDAV in **folder mode** — TorBox creates one
> folder per completed download, named after the release, with the media
> file(s) inside it:
>
> ```
> <WEBDAV_MOUNT_ROOT>/<release name>/<release>.mkv
> ```
>
> A **flat layout** (files written directly into the mount root with no
> per-release folder) is **not supported** — sab2torbox locates a release by
> its folder. If TorBox is configured for flat/local-files output, switch it
> back to folder mode.
>
> TorBox places these folders **directly under the mount root** — it does *not*
> create a `usenet/` subfolder. Leave `SAB2TORBOX_WEBDAV_USENET_SUBPATH` empty
> unless your particular mount nests releases under a subdirectory.

## Quickstart

### 1. Run the container

```yaml
# docker-compose.yml
services:
  sab2torbox:
    image: ghcr.io/radaiko/sab2torbox:latest
    container_name: sab2torbox
    restart: unless-stopped
    environment:
      - SAB2TORBOX_TORBOX_API_TOKEN=${TORBOX_API_TOKEN}
      - SAB2TORBOX_SAB_API_KEY=${SAB_API_KEY}
      - SAB2TORBOX_WEBDAV_MOUNT_ROOT=/mnt/torbox
      - SAB2TORBOX_SYMLINK_ROOT=/mnt/smedia/_incoming
      - TZ=Europe/Vienna
    ports:
      - "8181:8080"
    volumes:
      - ./config:/config
      - type: bind
        source: /mnt/torbox
        target: /mnt/torbox
        bind:
          propagation: rslave
      - type: bind
        source: /mnt/smedia
        target: /mnt/smedia
        bind:
          propagation: rslave
```

`./config` must be writable by uid **65532** (the distroless `nonroot` user).
Mount `/mnt/torbox` and `/mnt/smedia` into the Sonarr/Radarr (and Plex/Jellyfin)
containers at the same paths. A full example is in
[`deploy/docker-compose.yml`](deploy/docker-compose.yml).

### 2. Configure Sonarr / Radarr

Settings → Download Clients → **+** → **SABnzbd**:

| Field | Value |
|---|---|
| Host | `sab2torbox` (compose service name) or the host IP |
| Port | `8181` (the host-mapped port) |
| API Key | your `SAB2TORBOX_SAB_API_KEY` |
| Category | `sonarr` (Radarr: `radarr`) |

Repeat for each app. The category must be in `SAB2TORBOX_CATEGORIES`.

### 3. rclone WebDAV mount

sab2torbox does not mount anything itself — it expects TorBox's WebDAV to
already be rclone-mounted at `WEBDAV_MOUNT_ROOT` on the host. Reference mount:

```sh
rclone mount torbox-webdav: /mnt/torbox \
  --vfs-cache-mode full \
  --vfs-cache-max-size 50G \
  --dir-cache-time 10s \
  --vfs-read-chunk-size 32M \
  --uid 65532 --gid 65532 \
  --allow-other
```

`--dir-cache-time 10s` keeps directory listings fresh so completed downloads
appear quickly; sab2torbox additionally retries the expected path for up to 30s.

## Configuration

All configuration is via `SAB2TORBOX_*` environment variables.

| Variable | Required | Default | Description |
|---|---|---|---|
| `SAB2TORBOX_TORBOX_API_TOKEN` | yes | — | TorBox API token (torbox.app/settings) |
| `SAB2TORBOX_SAB_API_KEY` | yes | — | API key Sonarr/Radarr authenticate with |
| `SAB2TORBOX_WEBDAV_MOUNT_ROOT` | yes | — | Host path of the rclone WebDAV mount |
| `SAB2TORBOX_WEBDAV_USENET_SUBPATH` | no | _(empty)_ | Subpath under the mount root where release folders appear; empty = mount root |
| `SAB2TORBOX_SYMLINK_ROOT` | yes | — | Root of the symlink farm; must share a filesystem with the media library |
| `SAB2TORBOX_LISTEN_ADDR` | no | `:8080` | HTTP bind address |
| `SAB2TORBOX_DATABASE_PATH` | no | `/config/sab2torbox.db` | SQLite database path |
| `SAB2TORBOX_POLL_INTERVAL` | no | `1m` | How often to poll TorBox for in-flight jobs |
| `SAB2TORBOX_LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error` |
| `SAB2TORBOX_CATEGORIES` | no | `sonarr,radarr,sonarr-anime` | Comma-separated allowed categories |
| `SAB2TORBOX_TORBOX_WEBDAV_USER` | no | — | TorBox WebDAV username — set with `_PASS` to enable the WebDAV refresh |
| `SAB2TORBOX_TORBOX_WEBDAV_PASS` | no | — | TorBox WebDAV password (normally your API token) |
| `SAB2TORBOX_TORBOX_WEBDAV_REFRESH_URL` | no | `https://webdav.torbox.app/refresh` | Endpoint hit to force a WebDAV refresh |
| `SAB2TORBOX_TORBOX_WEBDAV_REFRESH_COOLDOWN` | no | `2m` | Minimum gap between forced refreshes |

## Speeding up completion (optional WebDAV refresh)

TorBox's WebDAV listing refreshes only **every ~15 minutes** — a deliberate
limit, since listing is database-heavy. So a finished download can take up to
15 minutes to appear on the mount, and Sonarr can't import until it does.

sab2torbox can cut that to seconds. Set `SAB2TORBOX_TORBOX_WEBDAV_USER` and
`SAB2TORBOX_TORBOX_WEBDAV_PASS` (your TorBox WebDAV credentials — the password
is normally your API token) and it will hit TorBox's `/refresh` endpoint to
force the listing to update.

To stay a good citizen of a deliberately rate-limited endpoint, the refresh:

- **only fires once every active download has finished** — while anything is
  still transferring, TorBox's own 15-minute cycle is left to handle it;
- is **debounced** by `SAB2TORBOX_TORBOX_WEBDAV_REFRESH_COOLDOWN` (default 2m);
- **backs off for 15 minutes** if TorBox returns HTTP 429.

Leave the credentials unset to disable the feature entirely.

## Auto-healing rotated releases

TorBox rotates releases out of its storage after roughly 30 days to reclaim
space. When that happens, the symlink your library holds — the one Sonarr moved
into the `tv/Show/...` folder — still points at the old release folder on the
WebDAV mount, but that folder is now gone. Plex/Jellyfin follows the symlink,
finds nothing, and shows the item as "Unplayable". Sonarr itself never sees
a missing state because its import already succeeded.

With `SAB2TORBOX_HEAL_ENABLED=true` and `SAB2TORBOX_HEAL_LIBRARY_ROOTS` set to
the comma-separated Sonarr/Radarr library roots (e.g.
`/mnt/smedia/tv,/mnt/smedia/movies`), an hourly healer:

1. **Walks** every configured library root and records any symlink whose target
   sits inside `WEBDAV_MOUNT_ROOT` and maps to a known job.
2. **Detects** broken symlinks — ones whose target has disappeared from the
   WebDAV mount — and flags them.
3. **Resubmits** the original stored NZB to TorBox for each affected job.
   TorBox usually recognises the file as a cache hit and makes it available
   within seconds, so no new download is needed.
4. **Atomically repoints** each flagged symlink at the new release folder once
   TorBox signals completion. The rename is POSIX-atomic, so any process with
   the file open continues reading without interruption. Sonarr never sees the
   file as missing.

### Configuration

| Variable | Default | Description |
|---|---|---|
| `SAB2TORBOX_HEAL_ENABLED` | `false` | Master switch; set to `true` to enable the healer |
| `SAB2TORBOX_HEAL_INTERVAL` | `1h` | How often the healer walks the library roots |
| `SAB2TORBOX_HEAL_LIBRARY_ROOTS` | _(required when enabled)_ | Comma-separated absolute paths to walk (your Sonarr/Radarr library roots) |
| `SAB2TORBOX_HEAL_DRY_RUN` | `false` | Detect and log broken symlinks but never modify anything |
| `SAB2TORBOX_HEAL_MAX_ATTEMPTS` | `3` | Give up healing a job after this many consecutive failures |
| `SAB2TORBOX_HEAL_BACKOFF_INITIAL` | `5m` | Exponential backoff base between failed heal attempts |
| `SAB2TORBOX_HEAL_WEBHOOK_URL` | _(empty)_ | URL that receives a JSON POST on heal events; empty disables the webhook. |
| `SAB2TORBOX_HEAL_WEBHOOK_EVENTS` | `failed` | Comma-separated subset of `detected,healing,healed,failed` to notify on. |

### Storage cost of keeping NZB content

sab2torbox keeps the raw NZB file (the heal seed) for each job's entire
lifetime — it is needed to resubmit to TorBox. NZBs are typically tens of
kilobytes; 10,000 jobs add up to roughly 30 MB of database storage, which is
negligible.

### Monitoring and manual control

Four HTTP endpoints let you observe and steer the healer without restarting the service:

- **`GET /health/symlinks`** — JSON object with counts: `tracked`, `broken`, `healing`, `heal_failed`, plus `last_run` and `next_run` timestamps. A quick way to see the overall heal state.
- **`GET /health/heal_failed`** — JSON array of jobs the healer has given up on (their `heal_count` has reached `HEAL_MAX_ATTEMPTS`). Each entry contains `job_id`, `name`, `broken_symlinks`, `last_heal_error`, `heal_count`, and `last_healed_at`.
- **`POST /health/heal/{job_id}/retry`** — resets that job's `heal_count` to zero so the healer retries it on its next tick. Use this after confirming the NZB should be resolvable again (e.g. TorBox found the content on a different server).
- **`POST /health/heal/{job_id}/give_up`** — marks the job `manually_resolved` and stops tracking its symlinks; the healer ignores it from that point on. Use when you have re-acquired the release through Sonarr and no longer want sab2torbox to attempt a heal.

**Webhook notifications.** When `SAB2TORBOX_HEAL_WEBHOOK_URL` is set, sab2torbox POSTs a `Content-Type: application/json` body to that URL on each configured heal event. The JSON body always contains `event` (one of `detected`, `healing`, `healed`, `failed`), `timestamp` (RFC 3339), and a `job` object with `id`, `name`, `category`, and `heal_count`. Depending on the event, the body may also include `symlinks_healed` (count of atomically repointed symlinks), `new_torbox_id` (the TorBox download ID after the heal), and `error` (the failure message on a `failed` event). Delivery is **best-effort** — failures are logged but never retried. The webhook is **unauthenticated**; if you need auth, put a reverse proxy in front of the receiving endpoint.

## Troubleshooting

**Files don't appear after a download completes.**
TorBox's WebDAV listing refreshes only every ~15 minutes, so a finished
download can take that long to surface on the mount. Enable the optional
WebDAV refresh (see *Speeding up completion* above) to force it within seconds.

**Downloads stay in the queue forever / `waiting for webdav path` in the logs.**
sab2torbox can't find the release folder. Either TorBox's WebDAV is in flat
mode (unsupported — switch it to folder mode), or `WEBDAV_USENET_SUBPATH` is
set wrong (it should normally be empty, since TorBox puts release folders
directly under the mount root).

**Sonarr: "download client places downloads in `…` but this directory does not
appear to exist inside the container."**
`SYMLINK_ROOT` (and so each `<SYMLINK_ROOT>/<category>/` directory) must be
bind-mounted into the Sonarr/Radarr container at the *same* path it has in
sab2torbox. sab2torbox pre-creates the category directories at startup, so they
exist as long as the mount is shared.

**Sonarr import is slow / copies bytes.**
The library is not on the same filesystem as `SYMLINK_ROOT`, so moving the
symlink dereferences it into a full copy from TorBox. Put the library and
`SYMLINK_ROOT` under one filesystem (e.g. both on `/mnt/smedia`).

**Imported files won't play / show as missing.**
The symlink targets are absolute `/mnt/torbox/...` paths. Every container that
*reads* the media — Sonarr for analysis, Plex/Jellyfin — must mount the WebDAV
path at that exact path, or the links dangle.

**Duplicate submissions.**
Expected — Sonarr retries sometimes. sab2torbox deduplicates by NZB SHA256
(`addfile`) or by URL (`addurl`), scoped to the category, and returns the
existing job ID instead of submitting twice.

**`/healthz` returns 503.**
The database is unreachable or the TorBox token is invalid/expired. The TorBox
check is cached for 5 minutes.

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

## Compared to TorBoxarr and Decypharr

- **[TorBoxarr](https://github.com/MrJoiny/TorBoxarr)** does a similar job but
  stages completed files to local disk in a `local_downloading` step so Sonarr
  has real local files. sab2torbox deliberately skips that — files stay on
  TorBox and are reached only through the WebDAV mount.
- **[Decypharr](https://github.com/sirrobot01/decypharr)** is a debrid/torrent
  bridge. It does not cover the Usenet-via-WebDAV flow. Use Decypharr for
  torrents; use sab2torbox for zero-copy Usenet imports.

## Roadmap & non-goals

Out of scope by design: torrent support (use Decypharr), a web UI (the TorBox
dashboard exists), manual NZB upload, NNTP fallback, repair/health-checking of
existing TorBox content, and multi-tenant operation.

## TorBox API quirks

Discovered while building against the TorBox v1 API:

- **IDs are inconsistently typed.** `usenetdownload_id` and `mylist` record IDs
  may serialize as either a JSON number or a quoted string. sab2torbox parses
  both via a defensive `FlexInt` type.
- **`progress` is a float 0.0–1.0**, not a 0–100 percentage.
- Completed downloads appear as one folder per release **directly under the
  mount root** — `<WEBDAV_MOUNT_ROOT>/<TorBox download name>/` — with the media
  file(s) inside. TorBox does not create a `usenet/` subfolder.
- TorBox's `name` is unstable across records (e.g. `DD51` vs `DD 51` vs
  `DD+51`), but the on-disk *folder* always matches the `name` that `mylist`
  returns, so sab2torbox resolves the folder by that name.
- The WebDAV listing refreshes only every ~15 minutes; hitting `/refresh`
  forces it (see *Speeding up completion*).

## License

MIT — see [LICENSE](LICENSE).
