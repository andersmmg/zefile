package metacache

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/krishna2206/zefile/internal/db"
	"github.com/krishna2206/zefile/internal/storage"
)

func open(t *testing.T) (*Service, func()) {
	t.Helper()
	database, err := db.Open(t.Context(), db.Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	svc := New(database.Write, time.Minute)
	return svc, func() { _ = database.Close() }
}

func mustPath(t *testing.T, s string) storage.Path {
	t.Helper()
	p, err := storage.ParsePath(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStoreLookupRoundTrip(t *testing.T) {
	svc, done := open(t)
	defer done()
	ctx := t.Context()

	p := mustPath(t, "/docs/report.pdf")
	mod := time.Now().UTC().Truncate(time.Microsecond)
	cached := mod.Add(time.Second)
	if err := svc.Store(ctx, p, Entry{Size: 42, ModTime: mod, IsDir: false, CachedAt: cached}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Lookup(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 42 || got.IsDir || !got.ModTime.Equal(mod) || !got.CachedAt.Equal(cached) {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	if _, err := svc.Lookup(ctx, mustPath(t, "/docs/missing.pdf")); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing row should be ErrNoRows, got %v", err)
	}
}

func TestHitValidity(t *testing.T) {
	svc, done := open(t)
	defer done()
	ctx := t.Context()

	now := time.Now().UTC().Truncate(time.Microsecond)
	old := now.Add(-time.Hour)

	// Freshly verified row hits with no listing stamp at all.
	fresh := mustPath(t, "/fresh.pdf")
	if err := svc.Store(ctx, fresh, Entry{Size: 1, ModTime: old, CachedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Hit(ctx, fresh, now); err != nil {
		t.Fatalf("freshly verified row should hit: %v", err)
	}

	// Old row with no stamp misses.
	stale := mustPath(t, "/dir/stale.pdf")
	if err := svc.Store(ctx, stale, Entry{Size: 1, ModTime: old, CachedAt: old}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Hit(ctx, stale, now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale unstamped row should miss, got %v", err)
	}

	// A fresh parent listing rescues the old row.
	if err := svc.ReplaceDir(ctx, mustPath(t, "/dir"), map[string]Entry{
		"stale.pdf": {Size: 1, ModTime: old, CachedAt: old},
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Hit(ctx, stale, now); err != nil {
		t.Fatalf("row under fresh listing should hit: %v", err)
	}
	if _, err := svc.Hit(ctx, stale, now.Add(2*time.Minute)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("row under stale listing should miss, got %v", err)
	}
}

func TestReplaceDirCollectsDeletions(t *testing.T) {
	svc, done := open(t)
	defer done()
	ctx := t.Context()

	dir := mustPath(t, "/docs")
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := svc.Store(ctx, mustPath(t, "/docs/gone.pdf"), Entry{Size: 9, ModTime: now, CachedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReplaceDir(ctx, dir, map[string]Entry{
		"a.pdf": {Size: 1, ModTime: now, CachedAt: now},
		"sub":   {ModTime: now, IsDir: true, CachedAt: now},
	}, now); err != nil {
		t.Fatal(err)
	}

	kids, err := svc.ChildrenHit(ctx, dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 2 || kids["a.pdf"].Size != 1 || !kids["sub"].IsDir {
		t.Fatalf("children mismatch: %v", kids)
	}
	if _, err := svc.Lookup(ctx, mustPath(t, "/docs/gone.pdf")); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted file should be collected, got %v", err)
	}
	if _, err := svc.ChildrenHit(ctx, dir, now.Add(2*time.Minute)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale listing should miss, got %v", err)
	}
	if _, err := svc.ChildrenHit(ctx, mustPath(t, "/never"), now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unstamped dir should miss, got %v", err)
	}
}

func TestInvalidateDirKeepsRowsInert(t *testing.T) {
	svc, done := open(t)
	defer done()
	ctx := t.Context()

	dir := mustPath(t, "/docs")
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := svc.Store(ctx, dir, Entry{ModTime: now, IsDir: true, CachedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReplaceDir(ctx, dir, map[string]Entry{
		"a.pdf": {Size: 1, ModTime: now, CachedAt: now.Add(-time.Hour)},
	}, now); err != nil {
		t.Fatal(err)
	}

	if err := svc.InvalidateDir(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ChildrenHit(ctx, dir, now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("invalidated dir should miss, got %v", err)
	}
	// The child row is old, so without the stamp it must not serve either.
	if _, err := svc.Hit(ctx, mustPath(t, "/docs/a.pdf"), now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unstamped old row should miss, got %v", err)
	}
	// But the directory's own freshly verified row still hits.
	if _, err := svc.Hit(ctx, dir, now); err != nil {
		t.Fatalf("own fresh row should hit: %v", err)
	}
}

func TestRelocateMovesSubtreeAndStamps(t *testing.T) {
	svc, done := open(t)
	defer done()
	ctx := t.Context()

	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, p := range []string{"/docs", "/docs/a.pdf", "/docs/sub", "/docs/sub/b.pdf"} {
		isDir := p == "/docs" || p == "/docs/sub"
		if err := svc.Store(ctx, mustPath(t, p), Entry{ModTime: now, IsDir: isDir, CachedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.ReplaceDir(ctx, mustPath(t, "/"), map[string]Entry{
		"docs":   {ModTime: now, IsDir: true, CachedAt: now},
		"target": {ModTime: now, IsDir: true, CachedAt: now},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReplaceDir(ctx, mustPath(t, "/docs/sub"), map[string]Entry{
		"b.pdf": {Size: 2, ModTime: now, CachedAt: now},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReplaceDir(ctx, mustPath(t, "/target"), map[string]Entry{}, now); err != nil {
		t.Fatal(err)
	}

	if err := svc.Relocate(ctx, mustPath(t, "/docs"), mustPath(t, "/target/docs")); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"/target/docs", "/target/docs/a.pdf", "/target/docs/sub/b.pdf"} {
		if _, err := svc.Lookup(ctx, mustPath(t, p)); err != nil {
			t.Fatalf("moved row %s missing: %v", p, err)
		}
	}
	for _, p := range []string{"/docs", "/docs/a.pdf", "/docs/sub/b.pdf"} {
		if _, err := svc.Lookup(ctx, mustPath(t, p)); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("old row %s should be gone, got %v", p, err)
		}
	}
	// The moved listing stamp travels with the directory.
	if _, err := svc.ChildrenHit(ctx, mustPath(t, "/target/docs/sub"), now); err != nil {
		t.Fatalf("moved stamp should keep listing fresh: %v", err)
	}
	// Both parents re-list.
	for _, p := range []string{"/", "/target"} {
		if _, err := svc.ChildrenHit(ctx, mustPath(t, p), now); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("parent %s should re-list, got %v", p, err)
		}
	}
}

func TestForgetSubtree(t *testing.T) {
	svc, done := open(t)
	defer done()
	ctx := t.Context()

	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, p := range []string{"/docs", "/docs/a.pdf", "/docs/sub/b.pdf", "/keep.pdf"} {
		if err := svc.Store(ctx, mustPath(t, p), Entry{ModTime: now, CachedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.ReplaceDir(ctx, mustPath(t, "/docs/sub"), map[string]Entry{
		"b.pdf": {Size: 1, ModTime: now, CachedAt: now},
	}, now); err != nil {
		t.Fatal(err)
	}

	if err := svc.Forget(ctx, mustPath(t, "/docs")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/docs", "/docs/a.pdf", "/docs/sub/b.pdf"} {
		if _, err := svc.Lookup(ctx, mustPath(t, p)); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("row %s should be gone, got %v", p, err)
		}
	}
	if _, err := svc.ChildrenHit(ctx, mustPath(t, "/docs/sub"), now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("subtree stamps should be gone, got %v", err)
	}
	if _, err := svc.Lookup(ctx, mustPath(t, "/keep.pdf")); err != nil {
		t.Fatalf("unrelated row should survive: %v", err)
	}
}
