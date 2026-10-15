-- Per-user references: an app-level "symlink" from an entry to a target path.

-- +goose Up
CREATE TABLE entry_references (
    path    TEXT    NOT NULL,
    parent  TEXT    NOT NULL,
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    target  TEXT    NOT NULL,
    PRIMARY KEY (user_id, path)
) STRICT, WITHOUT ROWID;

CREATE INDEX entry_references_parent_idx ON entry_references (user_id, parent);

-- +goose Down
DROP TABLE entry_references;
