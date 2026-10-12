-- name: ListEntryColors :many
SELECT path, color FROM entry_colors WHERE user_id = ? AND parent = ?;

-- name: GetEntryColorsForPaths :many
-- One query for results spanning many folders.
SELECT path, color FROM entry_colors
WHERE user_id = ? AND path IN (sqlc.slice('paths'));

-- name: SetEntryColor :exec
INSERT INTO entry_colors (path, parent, user_id, color) VALUES (?, ?, ?, ?)
ON CONFLICT (user_id, path) DO UPDATE SET
    color = excluded.color,
    parent = excluded.parent;

-- name: DeleteEntryColor :execrows
DELETE FROM entry_colors WHERE user_id = ? AND path = ?;

-- name: DeleteEntryColorsSubtree :exec
-- Prefix comparison rather than LIKE, so names holding % or _ need no escaping.
DELETE FROM entry_colors
WHERE path = sqlc.arg(path)
   OR substr(path, 1, length(sqlc.arg(path)) + 1) = sqlc.arg(path) || '/';

-- name: MoveEntryColorsSubtree :exec
-- A renamed subtree keeps its colours, and the stored parent is rewritten with
-- it so the directory lookup stays correct underneath.
UPDATE entry_colors
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
