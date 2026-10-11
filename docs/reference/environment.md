# Environment variables

Every setting comes from the environment and is validated at startup.

## Application

### `ZEFILE_APP_URL`

**Required.** The public address of the application origin (the UI and API),
e.g. `https://zefile.example.tld`. Used to build links and to decide whether
session cookies may be marked `Secure` — cookies are `Secure` when this URL uses
`https`.

### `ZEFILE_CONTENT_URL`

*Optional.* The public address of the content origin, on a **different host**
from `ZEFILE_APP_URL`. When set, files are served from this cookieless origin
and can be previewed in place. When unset, the instance runs in single-origin
mode: it hardens itself automatically but sends every file as a download.

Zefile refuses to start if this shares a host with `ZEFILE_APP_URL` — separate
origins are what keep an uploaded file from reaching a session.

## Storage

### `ZEFILE_DATA_DIR`

**Required.** The storage root — the directory users browse. Point it at a host
directory (a bind mount), not a Docker volume, to give Zefile the whole disk.

### `ZEFILE_CONFIG_DIR`

**Required.** Where the SQLite database lives. It must **not** be inside
`ZEFILE_DATA_DIR`; Zefile enforces this, because a database users can list is a
database they can download, password hashes included.

### `ZEFILE_RESERVE_BYTES`

*Optional.* Free space kept in hand before refusing writes. Below the threshold
the service goes read-only — reads, sign-in and deletes still work — rather than
letting the disk fill until SQLite can no longer write. Defaults to a built-in
reserve.

### `ZEFILE_READ_ONLY`

*Optional, default `false`.* When `true`, refuses every write regardless of free
space. Useful for maintenance or a read-only mirror.

### `ZEFILE_PREVIEW_MAX_BYTES`

*Optional, default `2MiB`.* Bounds server-side preview reads: text previews
are truncated past it, and thumbnail sources past it are refused in
object-storage mode (where every preview byte is fetched remotely). Accepts a
bare byte count or a `KiB`/`MiB`/`GiB` suffix.

### `ZEFILE_THUMB_MAX_BYTES`

*Optional, default `512MiB`.* Caps the source file a background thumbnail job
will fetch. Thumbnails themselves are small JPEGs kept under a reserved
prefix in the same storage; larger files keep their icon. Same size format.

### `ZEFILE_QUOTA_BYTES`

*Optional, default `0` (unlimited).* Caps the bytes of live files the instance
will hold: uploads that would push usage past it are refused with a “storage
quota exceeded” error, checked when the upload starts and again when it
publishes. Per-account limits are set by an admin under **Members** (zero
means unlimited) and are enforced the same way. Usage counts live files only —
trash and generated thumbnails don’t count — and the sidebar shows each
account its own usage against its own limit. Same size format.

::: tip Retention
How long the audit log and the trash are kept is **not** an environment setting —
it is configured at runtime under **Settings → Retention** (admin). See the
[audit log](/features/audit-log).
:::

## Object storage

*Optional.* Set `ZEFILE_STORAGE_BACKEND=s3` to store files in S3-compatible
object storage (AWS S3, Backblaze B2, Cloudflare R2, MinIO) instead of
`ZEFILE_DATA_DIR`. `ZEFILE_S3_BUCKET` and `ZEFILE_S3_REGION` are required in
this mode; `ZEFILE_S3_ENDPOINT` addresses a non-AWS provider, and
`ZEFILE_S3_ACCESS_KEY_ID` / `ZEFILE_S3_SECRET_ACCESS_KEY` carry static
credentials (unset means the ambient AWS chain). `ZEFILE_S3_PREFIX` scopes the
tree inside the bucket, `ZEFILE_S3_FORCE_PATH_STYLE` switches to path-style
addressing, `ZEFILE_S3_PUBLIC_ENDPOINT` is the bucket's address as clients see
it (for presigned URLs), `ZEFILE_S3_PART_SIZE` is the multipart part length
(default 64 MiB, min 5 MiB; the browser uploads several parts concurrently), and `ZEFILE_S3_PRESIGN_TTL` bounds minted
capability URLs (default 15 min).

### `ZEFILE_S3_META_TTL`

*Optional, default `1m`.* How long directory listings and file stats are served
from the metadata cache before one S3 request revalidates them. Changes made
through the app invalidate immediately; the TTL only bounds how stale rows for
changes made outside the app can get. Accepts a Go duration (`30s`, `5m`);
`0` disables expiry-based revalidation.

## Server

### `ZEFILE_LISTEN`

*Optional, default `:8080`.* The address the HTTP server binds.

## File ownership

There is nothing to configure, and no `PUID`/`PGID`. The container entrypoint
runs the server as **whoever owns the mounted data directory**, so a bind-mounted
folder is written by its own owner and the first upload never fails on a
permission error. Mount a directory you own and it works.

Two edge cases the entrypoint handles:

- **A fresh, empty mount** (Docker creates a missing bind-mount target owned by
  root) is claimed for the image's built-in unprivileged user automatically.
- **A root-owned directory that already holds files** is not reowned — Zefile
  will not run as root. `chown` it to any non-root user once
  (`chown -R 1000:1000 <host directory>`) and Zefile follows.

To pin a specific user regardless, set `user:` on the service in your compose
file; the entrypoint sees it is already unprivileged and adopts nothing.
