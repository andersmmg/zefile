-- Per-user colour labels on entries. Sparse: only labelled entries have a row.
-- The colour is an id from the interface's palette, not a raw value.

-- +goose Up
CREATE TABLE entry_colors (
    path    TEXT    NOT NULL,
    parent  TEXT    NOT NULL,
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    color   TEXT    NOT NULL,
    -- Two accounts may colour the same path differently, so the user is part
    -- of the key rather than an attribute of the row.
    PRIMARY KEY (user_id, path)
) STRICT, WITHOUT ROWID;

CREATE INDEX entry_colors_parent_idx ON entry_colors (user_id, parent);

-- +goose Down
DROP TABLE entry_colors;
