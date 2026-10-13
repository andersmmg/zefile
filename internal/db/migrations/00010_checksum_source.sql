-- Where a cached digest came from. 'compute' is this server hashing the
-- bytes, 'store' is a whole-object checksum the object store itself holds,
-- and 'client' is the browser's claim

-- +goose Up
ALTER TABLE checksums ADD COLUMN source TEXT NOT NULL DEFAULT 'compute';

-- +goose Down
-- SQLite cannot drop a column before 3.35 i guess
