-- name: ListReferences :many
SELECT path, target FROM entry_references WHERE user_id = ? AND parent = ?;

-- name: GetReferencesForPaths :many
-- One query for results spanning many folders.
SELECT path, target FROM entry_references
WHERE user_id = ? AND path IN (sqlc.slice('paths'));

-- name: SetReference :exec
INSERT INTO entry_references (path, parent, user_id, target) VALUES (?, ?, ?, ?)
ON CONFLICT (user_id, path) DO UPDATE SET
    target = excluded.target,
    parent = excluded.parent;

-- name: DeleteReference :execrows
DELETE FROM entry_references WHERE user_id = ? AND path = ?;

-- name: DeleteReferencesSubtree :exec
-- Prefix comparison rather than LIKE, so names holding % or _ need no escaping.
DELETE FROM entry_references
WHERE path = sqlc.arg(path)
   OR substr(path, 1, length(sqlc.arg(path)) + 1) = sqlc.arg(path) || '/';

-- name: MoveReferencesSubtree :exec
-- A renamed subtree keeps its references, and the stored parent is rewritten with
-- it so the directory lookup stays correct underneath.
UPDATE entry_references
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

-- name: UpdateReferencesTargetSubtree :exec
-- Follows a rename for every user: a reference pointing into the moved subtree
-- is rewritten to the new location. This is what makes a reference keep
-- tracking its target across a move.
UPDATE entry_references
SET target = sqlc.arg(to_path) || substr(target, length(sqlc.arg(from_path)) + 1)
WHERE target = sqlc.arg(from_path)
   OR substr(target, 1, length(sqlc.arg(from_path)) + 1) = sqlc.arg(from_path) || '/';
