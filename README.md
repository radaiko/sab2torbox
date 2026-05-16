# sab2torbox

A small Go service that **pretends to be SABnzbd** to Sonarr/Radarr but submits
NZBs to [TorBox](https://torbox.app)'s Usenet API instead of downloading them
locally. Completed files are surfaced through TorBox's WebDAV mount, so the
bytes only ever physically exist on TorBox's servers (cached locally by
rclone's VFS). When Sonarr "imports" a release, it moves files *within the same
rclone-mounted filesystem* — a rename, not a byte copy. The result is a
**zero-local-storage** Usenet bridge.

## How it works

```
   Sonarr/Radarr                sab2torbox                 TorBox
   ┌───────────┐  addfile/    ┌────────────┐  create     ┌─────────┐
   │  SABnzbd  ├─ addurl ────▶│  API layer │── usenet ───▶│ Usenet  │
   │  client   │◀─ queue/ ────┤  (SQLite)  │  download    │ backend │
   └─────┬─────┘  history     └─────▲──────┘              └────┬────┘
         │                          │ poll mylist              │
         │ import (rename)          └──────────────────────────┘
         ▼                                                      │
   ┌──────────────────────── rclone WebDAV mount ────────────────┘
   │  /mnt/torbox/usenet/<release>/   ◀── files appear here
   │  /mnt/torbox/media/...           ◀── Sonarr library (SAME mount)
   └──────────────────────────────────────────────────────────────
```

1. Sonarr submits an NZB to the SABnzbd-compatible API.
2. sab2torbox stores it and a background worker submits it to TorBox.
3. A poller watches TorBox's `mylist` until the download finishes.
4. On completion, sab2torbox resolves the file path on the WebDAV mount and
   reports the job as complete in the SAB `history` endpoint.
5. Sonarr imports from that path. If its library is on the same mount, the
   import is an instant rename.
6. When Sonarr deletes the download (with `del_files`), sab2torbox deletes it
   from TorBox too.

## ⚠️ Critical requirement: same filesystem

> **The Sonarr/Radarr library root MUST live on the same rclone WebDAV mount as
> `SAB2TORBOX_WEBDAV_MOUNT_ROOT`.**
>
> If the library is on a different filesystem, Sonarr's import becomes a full
> byte-for-byte copy — slow, and it defeats the entire zero-storage purpose.
> Keep both the Usenet download path and the media library under the one
> rclone mount (e.g. `/mnt/torbox/usenet/...` and `/mnt/torbox/media/...`).

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
```

`./config` must be writable by uid **65532** (the distroless `nonroot` user).
A full example is in [`deploy/docker-compose.yml`](deploy/docker-compose.yml).

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
| `SAB2TORBOX_WEBDAV_USENET_SUBPATH` | no | `usenet` | Subpath under the mount where downloads appear |
| `SAB2TORBOX_LISTEN_ADDR` | no | `:8080` | HTTP bind address |
| `SAB2TORBOX_DATABASE_PATH` | no | `/config/sab2torbox.db` | SQLite database path |
| `SAB2TORBOX_POLL_INTERVAL` | no | `10s` | How often to poll TorBox for in-flight jobs |
| `SAB2TORBOX_LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error` |
| `SAB2TORBOX_CATEGORIES` | no | `sonarr,radarr,sonarr-anime` | Comma-separated allowed categories |

## Troubleshooting

**Files don't appear after a download completes.**
TorBox flags a download complete a few seconds before its WebDAV listing
updates. sab2torbox retries the expected path for 30s, then logs a warning and
retries on the next poll. If files never appear, lower rclone's
`--dir-cache-time`, or check that `WEBDAV_USENET_SUBPATH` matches your mount
layout.

**Sonarr import is slow / copies bytes.**
The library root is not on the rclone WebDAV mount. Move it onto the same mount
as `WEBDAV_MOUNT_ROOT` so the import is a rename. See the warning above.

**Duplicate submissions.**
Expected — Sonarr retries sometimes. sab2torbox deduplicates by NZB SHA256
(`addfile`) or by URL (`addurl`), scoped to the category, and returns the
existing job ID instead of submitting twice.

**`/healthz` returns 503.**
The database is unreachable or the TorBox token is invalid/expired. The TorBox
check is cached for 5 minutes.

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
- Completed files appear on the WebDAV mount at
  `<usenet-subpath>/<TorBox download name>/`.

## License

MIT — see [LICENSE](LICENSE).
