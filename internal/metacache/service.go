// Package metacache keeps a small SQLite mirror of object-storage metadata
// so browsing a bucket does not pay a HEAD request per file. S3 ListObjects
// returns names, sizes and ETags but no reliable modification times, and a
// per-entry HEAD turns one directory listing into dozens of round trips —
// the pain behind slow S3 browsing. The cache stores what HEAD, LIST and
// PUT already told us; a directory whose sync stamp is older than the TTL
// is re-listed (one round trip, not one per file), and every mutation the
// app makes (upload, mkdir, move, trash, remove) maintains the cached rows
// alongside the S3 objects.
//
// S3 stays the source of truth: a missing or stale row is never fatal —
// the storage layer falls back to S3 and refreshes the row on the way
// through. Anything uncertain invalidates rather than writes.
//
// Validity has two legs: a row is served when its directory's listing is
// fresh (meta_sync stamp within the TTL) or when the row itself was
// verified within the TTL (cached_at). The stamp is the commit flag for a
// listing refresh — it is written last, so a crash mid-refresh leaves the
// directory unstamped and it simply re-lists.
package metacache

import (
	"context"
	"database/sql"
	"time"

	"github.com/krishna2206/zefile/internal/db/sqlcgen"
	"github.com/krishna2206/zefile/internal/storage"
)

// Service mirrors file metadata into the file_cache and meta_sync tables.
// All reads serve the single-writer connection's view; writers already
// serialise through it, and metadata queries are short.
type Service struct {
	q   *sqlcgen.Queries
	ttl time.Duration
}

// New returns a Service backed by db. A non-positive ttl disables
// expiry-based revalidation (entries are still evicted on mutation).
func New(db sqlcgen.DBTX, ttl time.Duration) *Service {
	return &Service{q: sqlcgen.New(db), ttl: ttl}
}

// Entry is a cached metadata row. CachedAt is when the row was last
// verified against S3; callers filling entries from fresh S3 responses set
// it to now. It aliases the storage type so *Service satisfies
// storage.MetaCache without an import cycle.
type Entry = storage.MetaEntry

// Lookup fetches the cached row for p. It reports sql.ErrNoRows when nothing
// is cached. Use Hit when serving reads: a bare row says nothing about
// whether it is still valid.
func (s *Service) Lookup(ctx context.Context, p storage.Path) (Entry, error) {
	return toEntry(s.q.GetFileMeta(ctx, p.String()))
}

// Hit returns the cached row for p when it may be served without touching
// S3: the parent listing is fresh, or the row itself was verified within
// the TTL. Otherwise it reports sql.ErrNoRows and the caller falls back to
// the backing store.
func (s *Service) Hit(ctx context.Context, p storage.Path, now time.Time) (Entry, error) {
	e, err := s.Lookup(ctx, p)
	if err != nil {
		return Entry{}, err
	}
	if s.ttl > 0 && now.Sub(e.CachedAt) < s.ttl {
		return e, nil
	}
	stamp, err := s.SyncedAt(ctx, p.Parent())
	if err != nil {
		if err == sql.ErrNoRows {
			return Entry{}, sql.ErrNoRows
		}
		return Entry{}, err
	}
	if s.Fresh(stamp, now) {
		return e, nil
	}
	return Entry{}, sql.ErrNoRows
}

// Children returns cached rows for the immediate children of dir, keyed by
// file name. Directories are filled by one listing and read back by one
// indexed query, so this is the fast path for List. Like Lookup it is a
// primitive: ChildrenHit adds the freshness check.
func (s *Service) Children(ctx context.Context, dir storage.Path) (map[string]Entry, error) {
	rows, err := s.q.ListFileMetaChildren(ctx, dir.String())
	if err != nil {
		return nil, err
	}
	out := make(map[string]Entry, len(rows))
	for _, row := range rows {
		e, err := toEntry(row, nil)
		if err != nil {
			return nil, err
		}
		out[row.Name] = e
	}
	return out, nil
}

// ChildrenHit returns the cached listing for dir when its sync stamp is
// fresh, else sql.ErrNoRows.
func (s *Service) ChildrenHit(ctx context.Context, dir storage.Path, now time.Time) (map[string]Entry, error) {
	stamp, err := s.SyncedAt(ctx, dir)
	if err != nil {
		return nil, err
	}
	if !s.Fresh(stamp, now) {
		return nil, sql.ErrNoRows
	}
	return s.Children(ctx, dir)
}

// SyncedAt returns when dir was last filled by a listing. It reports
// sql.ErrNoRows when the directory was never cached.
func (s *Service) SyncedAt(ctx context.Context, dir storage.Path) (time.Time, error) {
	row, err := s.q.GetMetaSync(ctx, dir.String())
	if err != nil {
		return time.Time{}, err
	}
	return time.UnixMicro(row.SyncedAt).UTC(), nil
}

// Fresh reports whether the directory listing stamped at syncedAt may be
// served without revalidation.
func (s *Service) Fresh(syncedAt time.Time, now time.Time) bool {
	if s.ttl <= 0 {
		return true
	}
	return now.Sub(syncedAt) < s.ttl
}

// ReplaceDir swaps in a complete fresh listing for dir: children observed
// in S3, stamped at t. Rows for files deleted outside the app are collected
// here; the stamp is written last so a crash leaves the directory to
// re-list rather than half-cached.
func (s *Service) ReplaceDir(ctx context.Context, dir storage.Path, kids map[string]Entry, t time.Time) error {
	if err := s.q.DeleteFileMetaChildren(ctx, dir.String()); err != nil {
		return err
	}
	for name, e := range kids {
		child, err := dir.Child(name)
		if err != nil {
			return err
		}
		if err := s.store(ctx, child, e); err != nil {
			return err
		}
	}
	return s.q.SetMetaSync(ctx, sqlcgen.SetMetaSyncParams{
		Path:     dir.String(),
		SyncedAt: t.UnixMicro(),
	})
}

// Store records authoritative metadata for p: values just PUT, HEADed,
// listed or copied server-side — never rows synthesised from thin air.
func (s *Service) Store(ctx context.Context, p storage.Path, e Entry) error {
	return s.store(ctx, p, e)
}

func (s *Service) store(ctx context.Context, p storage.Path, e Entry) error {
	isDir := int64(0)
	if e.IsDir {
		isDir = 1
	}
	return s.q.UpsertFileMeta(ctx, sqlcgen.UpsertFileMetaParams{
		Path:     p.String(),
		Parent:   p.Parent().String(),
		Name:     p.Name(),
		Size:     e.Size,
		ModTime:  e.ModTime.UnixMicro(),
		IsDir:    isDir,
		CachedAt: e.CachedAt.UnixMicro(),
	})
}

// Forget drops the cached rows for p and everything beneath it, plus any
// sync stamps there. It succeeds when nothing was cached.
func (s *Service) Forget(ctx context.Context, p storage.Path) error {
	if err := s.q.DeleteFileMetaSubtree(ctx, p.String()); err != nil {
		return err
	}
	return s.q.DeleteMetaSyncSubtree(ctx, p.String())
}

// InvalidateDir drops the sync stamp for dir so it re-lists on next open,
// after the directory's contents changed. The children rows stay as inert
// data: without a stamp they are never served, and the next listing
// replaces them.
func (s *Service) InvalidateDir(ctx context.Context, dir storage.Path) error {
	return s.q.DeleteMetaSync(ctx, dir.String())
}

// Relocate renames cached rows when a path moves: the row for from becomes
// the row for to, rows beneath from are rewritten beneath to, and sync
// stamps move along so a moved directory keeps its fresh listing. The
// source and destination parents hold listings that just changed
// membership, so their stamps are dropped and they re-list on next open.
func (s *Service) Relocate(ctx context.Context, from, to storage.Path) error {
	if err := s.q.MoveFileMetaSubtree(ctx, sqlcgen.MoveFileMetaSubtreeParams{
		ToPath:   to.String(),
		FromPath: from.String(),
	}); err != nil {
		return err
	}
	if err := s.q.MoveMetaSyncSubtree(ctx, sqlcgen.MoveMetaSyncSubtreeParams{
		ToPath:   to.String(),
		FromPath: from.String(),
	}); err != nil {
		return err
	}
	if err := s.InvalidateDir(ctx, from.Parent()); err != nil {
		return err
	}
	if to.Parent().String() == from.Parent().String() {
		return nil
	}
	return s.InvalidateDir(ctx, to.Parent())
}

func toEntry(row sqlcgen.FileCache, err error) (Entry, error) {
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		Size:     row.Size,
		ModTime:  time.UnixMicro(row.ModTime).UTC(),
		IsDir:    row.IsDir != 0,
		CachedAt: time.UnixMicro(row.CachedAt).UTC(),
	}, nil
}

// Compile-time check that the service plugs into the S3 backend.
var _ storage.MetaCache = (*Service)(nil)
