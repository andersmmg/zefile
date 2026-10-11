-- name: GetFileMeta :one
SELECT * FROM file_cache WHERE path = ?;

-- name: ListFileMetaChildren :many
SELECT * FROM file_cache WHERE parent = ? ORDER BY name;

-- name: UpsertFileMeta :exec
INSERT INTO file_cache (path, parent, name, size, mod_time, is_dir, cached_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (path) DO UPDATE SET
    parent = excluded.parent,
    name = excluded.name,
    size = excluded.size,
    mod_time = excluded.mod_time,
    is_dir = excluded.is_dir,
    cached_at = excluded.cached_at;

-- name: DeleteFileMetaChildren :exec
DELETE FROM file_cache WHERE parent = ?;

-- name: DeleteFileMetaSubtree :exec
-- Drops a path and everything beneath it, using a prefix comparison rather
-- than LIKE so names holding % or _ need no escaping.
DELETE FROM file_cache
WHERE path = sqlc.arg(path)
   OR substr(path, 1, length(sqlc.arg(path)) + 1) = sqlc.arg(path) || '/';

-- name: MoveFileMetaSubtree :exec
-- Follows a rename across a whole subtree in one statement. Only rows at or
-- beneath the source move; the moved directory's own parent is untouched.
UPDATE file_cache
SET path = sqlc.arg(to_path) || substr(path, length(sqlc.arg(from_path)) + 1),
    parent = CASE
        WHEN parent = sqlc.arg(from_path) THEN sqlc.arg(to_path)
        WHEN substr(parent, 1, length(sqlc.arg(from_path)) + 1) = sqlc.arg(from_path) || '/'
            THEN sqlc.arg(to_path) || substr(parent, length(sqlc.arg(from_path)) + 1)
        ELSE parent
    END
WHERE path = sqlc.arg(from_path)
   OR substr(path, 1, length(sqlc.arg(from_path)) + 1) = sqlc.arg(from_path) || '/'
   OR parent = sqlc.arg(from_path)
   OR substr(parent, 1, length(sqlc.arg(from_path)) + 1) = sqlc.arg(from_path) || '/';

-- name: MoveMetaSyncSubtree :exec
-- Follows a rename like MoveFileMetaSubtree, so a moved directory keeps
-- its fresh listing instead of paying a re-list on next open.
UPDATE meta_sync
SET path = sqlc.arg(to_path) || substr(path, length(sqlc.arg(from_path)) + 1)
WHERE path = sqlc.arg(from_path)
   OR substr(path, 1, length(sqlc.arg(from_path)) + 1) = sqlc.arg(from_path) || '/';

-- name: GetMetaSync :one
SELECT * FROM meta_sync WHERE path = ?;

-- name: SetMetaSync :exec
INSERT INTO meta_sync (path, synced_at) VALUES (?, ?)
ON CONFLICT (path) DO UPDATE SET synced_at = excluded.synced_at;

-- name: DeleteMetaSync :exec
DELETE FROM meta_sync WHERE path = ?;

-- name: DeleteMetaSyncSubtree :exec
DELETE FROM meta_sync
WHERE path = sqlc.arg(path)
   OR substr(path, 1, length(sqlc.arg(path)) + 1) = sqlc.arg(path) || '/';
