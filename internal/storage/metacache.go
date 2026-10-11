package storage

import (
	"context"
	"time"
)

// MetaEntry is one cached metadata row: what a HEAD or listing told us,
// plus when it was verified. The file's own ModTime cannot serve as the
// cache clock — an old file can be newly cached — so CachedAt records the
// verification time separately.
type MetaEntry struct {
	Size     int64
	ModTime  time.Time
	IsDir    bool
	CachedAt time.Time
}

// MetaCache mirrors object metadata so directory listings and stats can be
// served without a round trip per file. The backing store stays the source
// of truth: a missing or stale row is never fatal, the caller falls back to
// S3 and refreshes the row on the way through.
//
// A miss is reported as sql.ErrNoRows, so callers treat "uncached" and
// "never seen" identically. Implementations live outside this package (see
// internal/metacache); this interface exists so the S3 backend can use the
// cache without importing the database, which would be an import cycle.
type MetaCache interface {
	// Hit returns the row for p when it may be served without touching
	// the store: the parent listing is fresh, or the row itself was
	// verified within the TTL.
	Hit(ctx context.Context, p Path, now time.Time) (MetaEntry, error)

	// ChildrenHit returns the cached listing for dir when its sync stamp
	// is fresh. The rows are unfiltered: per-user permission filtering
	// still happens on every serve.
	ChildrenHit(ctx context.Context, dir Path, now time.Time) (map[string]MetaEntry, error)

	// ReplaceDir swaps in a complete fresh listing for dir.
	ReplaceDir(ctx context.Context, dir Path, kids map[string]MetaEntry, now time.Time) error

	// Store records authoritative metadata just observed in the store.
	Store(ctx context.Context, p Path, e MetaEntry) error

	// Forget drops cached rows for p and everything beneath it.
	Forget(ctx context.Context, p Path) error

	// InvalidateDir drops the listing stamp for dir so it re-lists on
	// next open, after the directory's contents changed.
	InvalidateDir(ctx context.Context, dir Path) error

	// Relocate renames cached rows when a path moves.
	Relocate(ctx context.Context, from, to Path) error
}
