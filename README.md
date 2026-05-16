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
   │  /mnt/torbox/<release>/          ◀── TorBox puts a folder here
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

## ⚠️ Critical requirement: imports must be a rename

> Sonarr's import has to be a **rename within one filesystem**, never a
> byte-for-byte copy — otherwise it is slow and storage is no longer zero.
> What must share a filesystem depends on the mode (see *Storage modes* below):
>
> - **Direct mode** — the Sonarr/Radarr library must sit on the rclone WebDAV
>   mount (`SAB2TORBOX_WEBDAV_MOUNT_ROOT`).
> - **Symlink-farm mode** — the library must sit on the same filesystem as
>   `SAB2TORBOX_SYMLINK_ROOT`.

## Storage modes

sab2torbox can hand a completed download to Sonarr/Radarr two ways.

### Direct mode (default)

`SAB2TORBOX_SYMLINK_ROOT` unset. sab2torbox reports the TorBox release folder
*on the rclone WebDAV mount* as the `storage` path, and Sonarr imports by
moving files within that mount. Simple, but the library must live on the
WebDAV mount itself.

### Symlink-farm mode

Set `SAB2TORBOX_SYMLINK_ROOT` (e.g. `/mnt/smedia/_incoming`). On completion
sab2torbox creates one symlink per file under
`<SYMLINK_ROOT>/<category>/<release>/`, each pointing at the real file on the
WebDAV mount, and reports that directory as `storage`. Sonarr moves the tiny
*symlink* into its library, so the library only has to share a filesystem with
`SYMLINK_ROOT` — not with the WebDAV mount.

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
| `SAB2TORBOX_WEBDAV_USENET_SUBPATH` | no | _(empty)_ | Subpath under the mount root where release folders appear; empty = mount root |
| `SAB2TORBOX_SYMLINK_ROOT` | no | _(empty)_ | Enables symlink-farm mode (see *Storage modes*); empty = direct mode |
| `SAB2TORBOX_LISTEN_ADDR` | no | `:8080` | HTTP bind address |
| `SAB2TORBOX_DATABASE_PATH` | no | `/config/sab2torbox.db` | SQLite database path |
| `SAB2TORBOX_POLL_INTERVAL` | no | `10s` | How often to poll TorBox for in-flight jobs |
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
Two causes: (1) `WEBDAV_USENET_SUBPATH` points at a folder that doesn't exist —
leave it empty so `complete_dir` is the mount root itself; (2) `WEBDAV_MOUNT_ROOT`
must be bind-mounted into the Sonarr/Radarr container at the *same* path it has
in sab2torbox. sab2torbox reports no per-category subfolders, so Sonarr only
needs the mount root to exist — not a `<mount>/<category>` directory.

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
