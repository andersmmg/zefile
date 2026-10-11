-- Cached object-storage metadata, so directory listings and stats can be
-- served without a round trip. S3 stays the source of truth: rows are filled
-- lazily per directory, refreshed past their TTL, and maintained on every
-- mutation the app makes. Anything uncertain invalidates rather than writes.

-- +goose Up
-- cached_at is when this row was last verified against S3 (unix micros).
-- A row is served when its directory listing is fresh (meta_sync) or the
-- row itself was verified within the TTL — the file's own mod_time cannot
-- serve as a cache clock, since an old file can be newly cached.
CREATE TABLE file_cache (
    path      TEXT PRIMARY KEY,
    parent    TEXT NOT NULL,
    name      TEXT NOT NULL,
    size      INTEGER NOT NULL DEFAULT 0,
    mod_time  INTEGER NOT NULL DEFAULT 0,
    is_dir    INTEGER NOT NULL DEFAULT 0,
    cached_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_file_cache_parent ON file_cache (parent);

-- One row per directory the cache has listed, with the time it was filled.
CREATE TABLE meta_sync (
    path      TEXT PRIMARY KEY,
    synced_at INTEGER NOT NULL
);
