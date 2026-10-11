# Deployment

Zefile is one container behind a reverse proxy that terminates TLS. The only
Zefile-specific consideration is the two origins.

## Reverse proxy

Point both hostnames at the same container port (`8080`). Zefile decides which
role a request plays from its `Host` header, so no path rules are needed.

```
zefile.example.tld  ─┐
                     ├─►  zefile:8080
dl.example.tld      ─┘
```

Any proxy works (Caddy, Traefik, nginx). Terminate TLS at the proxy and forward
plain HTTP to the container. Because Zefile derives `Secure` cookies from
`ZEFILE_APP_URL`, make sure that variable uses `https`.

## Dokploy

Zefile deploys cleanly on [Dokploy](https://dokploy.com). Use the reference
compose file, set the two domains in the Dokploy UI, and point the storage
volume at a host directory. Dokploy handles TLS and routing for both hostnames.

## Forwarded headers

Zefile deliberately ignores `X-Forwarded-For` for rate limiting and audit: it is
trivially forged, and trusting it without knowing the proxy in front would let
anyone reset their own rate limit. If you need accurate client IPs in the audit
log, this is a known trade-off favouring safety.

## Object storage (S3 / Backblaze B2)

In S3 mode (`ZEFILE_STORAGE_BACKEND: s3`) the browser talks to the bucket
directly: part uploads are `PUT`s and downloads/previews are `GET`s against
presigned URLs. Deletes never touch the bucket from the browser — they go
through the server API. So the bucket needs a CORS rule covering exactly that:
`GET` + `PUT` + `HEAD` from the app origin.

The repository ships the rule as B2-native JSON in `deploy/b2-cors.json`:

```json
[
  {
    "corsRuleName": "zefile-dev",
    "allowedHeaders": ["*"],
    "allowedOperations": ["s3_get", "s3_put", "s3_head"],
    "allowedOrigins": ["http://localhost:8080"],
    "exposeHeaders": ["ETag", "Content-Length", "Content-Range", "Accept-Ranges"],
    "maxAgeSeconds": 3600
  }
]
```

Why it looks like this:

- `s3_put` covers part uploads; `s3_get` + `s3_head` cover downloads, ranged
  previews, and metadata checks.
- The origin is the app origin as the browser sees it. The shipped file targets
  local dev (`http://localhost:8080`) — add your production origin (e.g.
  `https://zefile.example.tld`) when you deploy.
- `allowedHeaders: ["*"]` because part `PUT`s carry the file's `Content-Type`
  and previews send `Range`; enumerating every content type is brittle.
- `exposeHeaders` keeps `ETag` and the range headers readable to JavaScript.

Apply it with the Backblaze CLI (`bbb2` here — the `b2` name belongs to an
unrelated build tool on some systems):

```sh
# Authorize once; afterwards the session is cached.
bbb2 account authorize <applicationKeyId> <applicationKey>

# bucketType is positional and required — repeat the bucket's current type
# (allPrivate) so the update preserves it.
bbb2 bucket update --cors-rules "$(<deploy/b2-cors.json)" <bucketName> allPrivate
```

Verify:

```sh
bbb2 bucket get <bucketName>
```

Aborted uploads leave multipart parts behind that you pay for. Set a lifecycle
rule aborting incomplete multipart uploads (B2: bucket → Lifecycle Settings,
7 days is sane).

| Provider | Endpoint | Region |
|---|---|---|
| Backblaze B2 | `https://s3.<region>.backblazeb2.com` | your bucket's region, e.g. `us-west-004` |
| AWS S3 | unset (AWS default) | the bucket's region |
| R2 / MinIO | your endpoint URL | as configured (`auto` on R2) |

Cost note: downloads and previews stream straight from the bucket, so every
view is bucket egress — there is no server-side cache to absorb repeat views.

## Backups

Back up two things separately: your **data directory** (the files — use your
usual disk backup) and the **database**, which holds every account, permission,
share and token. Losing the database loses all of that even though the files
survive, so it is the critical half.

Zefile has built-in commands for the database. `backup` is safe to run while the
server is live — it takes a consistent snapshot with SQLite's `VACUUM INTO`:

```sh
docker compose exec zefile zefile backup
# → /config/backups/zefile-2026-08-03-020000.db
```

Pass a path to choose where it lands — handy for a nightly cron:

```sh
docker compose exec zefile zefile backup /config/backups/nightly.db
```

The snapshot is a self-contained SQLite file you can copy off the host.

To restore, **stop the server first** — replacing the database under a running
instance would corrupt it. `restore` validates the snapshot and copies the
current database aside (as `zefile.db.pre-restore-…`) before replacing it:

```sh
docker compose stop zefile
docker compose run --rm zefile zefile restore /config/backups/nightly.db
docker compose start zefile
```

If the restored database references files that no longer exist on disk — because
the snapshot predates changes to the tree — `restore` lists them, so you know
what drifted. Schema migrations also snapshot the database automatically before
they run, so an upgrade always has a point to roll back to.
