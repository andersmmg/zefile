package storage

// These tests prove the S3 backend actually uses its MetaCache: after
// priming, the stub server is shut down, so any HTTP round trip fails and
// only cache-served operations succeed.

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeMeta is a script-free in-memory MetaCache with the same validity rule
// as the real one: a row serves when it was verified within the TTL or its
// directory listing is fresh.
type fakeMeta struct {
	ttl    time.Duration
	rows   map[string]MetaEntry
	stamps map[string]time.Time
	calls  []string
}

func newFakeMeta(ttl time.Duration) *fakeMeta {
	return &fakeMeta{ttl: ttl, rows: map[string]MetaEntry{}, stamps: map[string]time.Time{}}
}

func (f *fakeMeta) log(name string) { f.calls = append(f.calls, name) }

func (f *fakeMeta) fresh(t time.Time, now time.Time) bool {
	return f.ttl > 0 && now.Sub(t) < f.ttl
}

func (f *fakeMeta) Hit(_ context.Context, p Path, now time.Time) (MetaEntry, error) {
	f.log("Hit " + p.String())
	e, ok := f.rows[p.String()]
	if !ok {
		return MetaEntry{}, sql.ErrNoRows
	}
	if f.fresh(e.CachedAt, now) {
		return e, nil
	}
	if s, ok := f.stamps[p.Parent().String()]; ok && f.fresh(s, now) {
		return e, nil
	}
	return MetaEntry{}, sql.ErrNoRows
}

func (f *fakeMeta) ChildrenHit(_ context.Context, dir Path, now time.Time) (map[string]MetaEntry, error) {
	f.log("ChildrenHit " + dir.String())
	s, ok := f.stamps[dir.String()]
	if !ok || !f.fresh(s, now) {
		return nil, sql.ErrNoRows
	}
	out := map[string]MetaEntry{}
	for path, e := range f.rows {
		p, err := ParsePath(path)
		if err != nil || p.Parent().String() != dir.String() {
			continue
		}
		out[p.Name()] = e
	}
	return out, nil
}

func (f *fakeMeta) ReplaceDir(_ context.Context, dir Path, kids map[string]MetaEntry, now time.Time) error {
	f.log("ReplaceDir " + dir.String())
	for path := range f.rows {
		p, err := ParsePath(path)
		if err != nil {
			continue
		}
		if p.Parent().String() == dir.String() {
			delete(f.rows, path)
		}
	}
	for name, e := range kids {
		child, err := dir.Child(name)
		if err != nil {
			return err
		}
		f.rows[child.String()] = e
	}
	f.stamps[dir.String()] = now
	return nil
}

func (f *fakeMeta) Store(_ context.Context, p Path, e MetaEntry) error {
	f.log("Store " + p.String())
	f.rows[p.String()] = e
	return nil
}

func (f *fakeMeta) Forget(_ context.Context, p Path) error {
	f.log("Forget " + p.String())
	prefix := p.String()
	if !p.IsRoot() {
		prefix += "/"
	}
	for path := range f.rows {
		if path == p.String() || len(path) > len(prefix) && path[:len(prefix)] == prefix {
			delete(f.rows, path)
		}
	}
	for stamp := range f.stamps {
		if stamp == p.String() || len(stamp) > len(prefix) && stamp[:len(prefix)] == prefix {
			delete(f.stamps, stamp)
		}
	}
	return nil
}

func (f *fakeMeta) InvalidateDir(_ context.Context, dir Path) error {
	f.log("InvalidateDir " + dir.String())
	delete(f.stamps, dir.String())
	return nil
}

func (f *fakeMeta) Relocate(_ context.Context, from, to Path) error {
	f.log("Relocate " + from.String() + "->" + to.String())
	fromStr, toStr := from.String(), to.String()
	for path, e := range f.rows {
		rel, ok := cutPrefix(path, fromStr)
		if !ok {
			continue
		}
		delete(f.rows, path)
		f.rows[toStr+rel] = e
	}
	for stamp, t := range f.stamps {
		rel, ok := cutPrefix(stamp, fromStr)
		if !ok {
			continue
		}
		delete(f.stamps, stamp)
		f.stamps[toStr+rel] = t
	}
	// Parent listings change membership, like the real implementation.
	delete(f.stamps, from.Parent().String())
	if to.Parent().String() != from.Parent().String() {
		delete(f.stamps, to.Parent().String())
	}
	return nil
}

func cutPrefix(path, prefix string) (string, bool) {
	if path == prefix {
		return "", true
	}
	if len(path) > len(prefix) && path[:len(prefix)] == prefix && path[len(prefix)] == '/' {
		return path[len(prefix):], true
	}
	return "", false
}

// newCachedTestS3 opens a backend against a stub server the test controls,
// so it can shut the server down and prove reads come from the cache.
func newCachedTestS3(t *testing.T, stub *stubS3, meta MetaCache) (*S3, func()) {
	t.Helper()
	srv := httptest.NewServer(stub)
	s, err := OpenS3(S3Config{
		Bucket:          "test-bucket",
		Region:          "us-east-1",
		Endpoint:        srv.URL,
		ForcePathStyle:  true,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		MetaCache:       meta,
	})
	if err != nil {
		srv.Close()
		t.Fatalf("OpenS3: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, srv.Close
}

func TestS3StatHitNeedsNoHTTP(t *testing.T) {
	fake := newFakeMeta(time.Hour)
	now := time.Now()
	p, _ := ParsePath("/report.pdf")
	mod := now.Add(-time.Hour)
	fake.rows[p.String()] = MetaEntry{Size: 5, ModTime: mod, CachedAt: now}

	s, closeServer := newCachedTestS3(t, newStubS3(), fake)
	closeServer() // any round trip now fails

	info, err := s.Stat(t.Context(), p)
	if err != nil {
		t.Fatalf("cached Stat: %v", err)
	}
	if info.Size != 5 || !info.ModTime.Equal(mod) || info.IsDir {
		t.Fatalf("cached Stat mismatch: %+v", info)
	}
}

func TestS3StatMissStoresThenHits(t *testing.T) {
	fake := newFakeMeta(time.Hour)
	stub := newStubS3()
	stub.put("report.pdf", 5)

	s, closeServer := newCachedTestS3(t, stub, fake)
	p, _ := ParsePath("/report.pdf")

	first, err := s.Stat(t.Context(), p)
	if err != nil {
		t.Fatalf("live Stat: %v", err)
	}
	if _, ok := fake.rows[p.String()]; !ok {
		t.Fatal("miss should store the row")
	}

	closeServer()
	second, err := s.Stat(t.Context(), p)
	if err != nil {
		t.Fatalf("cached Stat: %v", err)
	}
	if second.Size != first.Size || !second.ModTime.Equal(first.ModTime) {
		t.Fatalf("cached %+v differs from live %+v", second, first)
	}
}

func TestS3StatMissingForgetsStaleRow(t *testing.T) {
	fake := newFakeMeta(time.Hour)
	p, _ := ParsePath("/gone.pdf")
	fake.rows[p.String()] = MetaEntry{Size: 5, CachedAt: time.Now().Add(-2 * time.Hour)}

	s, _ := newCachedTestS3(t, newStubS3(), fake)
	if _, err := s.Stat(t.Context(), p); !errors.Is(err, ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
	if _, ok := fake.rows[p.String()]; ok {
		t.Fatal("confirmed-missing path should be forgotten")
	}
}

func TestS3ListHitNeedsNoHTTP(t *testing.T) {
	fake := newFakeMeta(time.Hour)
	now := time.Now()
	dir, _ := ParsePath("/docs")
	fake.stamps[dir.String()] = now
	for name, size := range map[string]int64{"c.pdf": 3, "a.pdf": 1, "b.pdf": 2} {
		child, _ := dir.Child(name)
		fake.rows[child.String()] = MetaEntry{Size: size, ModTime: now, CachedAt: now}
	}
	other, _ := ParsePath("/docs-extra.pdf")
	fake.rows[other.String()] = MetaEntry{Size: 9, CachedAt: now}

	s, closeServer := newCachedTestS3(t, newStubS3(), fake)
	closeServer()

	entries, err := s.List(t.Context(), dir)
	if err != nil {
		t.Fatalf("cached List: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %+v", entries)
	}
	// Stable name order despite the map.
	for i, want := range []string{"a.pdf", "b.pdf", "c.pdf"} {
		if entries[i].Name != want {
			t.Fatalf("order mismatch: %+v", entries)
		}
	}
	if entries[0].Size != 1 || entries[2].Size != 3 {
		t.Fatalf("sizes mismatch: %+v", entries)
	}
}

func TestS3ListMissReplacesThenHits(t *testing.T) {
	fake := newFakeMeta(time.Hour)
	stub := newStubS3("docs/", "docs/a.pdf", "docs/b.pdf")

	s, closeServer := newCachedTestS3(t, stub, fake)
	dir, _ := ParsePath("/docs")

	live, err := s.List(t.Context(), dir)
	if err != nil {
		t.Fatalf("live List: %v", err)
	}
	if _, ok := fake.stamps[dir.String()]; !ok {
		t.Fatal("miss should stamp the directory")
	}

	closeServer()
	cached, err := s.List(t.Context(), dir)
	if err != nil {
		t.Fatalf("cached List: %v", err)
	}
	if len(cached) != len(live) {
		t.Fatalf("cached %d entries differs from live %d", len(cached), len(live))
	}
}

func TestS3MkdirStoresAndInvalidatesParent(t *testing.T) {
	fake := newFakeMeta(time.Hour)
	now := time.Now()
	root, _ := ParsePath("/")
	fake.stamps[root.String()] = now // a fresh root listing…

	s, _ := newCachedTestS3(t, newStubS3(), fake)
	p, _ := ParsePath("/newdir")
	if err := s.Mkdir(t.Context(), p); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	e, ok := fake.rows[p.String()]
	if !ok || !e.IsDir {
		t.Fatalf("Mkdir should store the directory row: %v", fake.rows)
	}
	if _, ok := fake.stamps[root.String()]; ok {
		t.Fatal("Mkdir should invalidate the parent listing")
	}
}

func TestS3MoveRelocatesRows(t *testing.T) {
	fake := newFakeMeta(time.Hour)
	now := time.Now()
	from, _ := ParsePath("/a.txt")
	to, _ := ParsePath("/sub/b.txt")
	sub, _ := ParsePath("/sub")
	fake.rows[from.String()] = MetaEntry{Size: 4, ModTime: now, CachedAt: now}
	fake.rows[sub.String()] = MetaEntry{IsDir: true, CachedAt: now}
	fake.stamps["/"] = now
	fake.stamps[sub.String()] = now

	stub := newStubS3()
	stub.put("a.txt", 4)
	stub.put("sub/", 0)
	s, closeServer := newCachedTestS3(t, stub, fake)

	if err := s.Move(t.Context(), from, to); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, ok := fake.rows[from.String()]; ok {
		t.Fatal("source row should be gone")
	}
	if _, ok := fake.rows[to.String()]; !ok {
		t.Fatal("destination row should exist")
	}
	if _, ok := fake.stamps[sub.String()]; ok {
		t.Fatal("destination parent should re-list")
	}

	// The moved file stats from cache with the server gone.
	closeServer()
	info, err := s.Stat(t.Context(), to)
	if err != nil {
		t.Fatalf("cached Stat after Move: %v", err)
	}
	if info.Size != 4 {
		t.Fatalf("size mismatch: %+v", info)
	}
}

func TestS3RemoveForgetsSubtree(t *testing.T) {
	fake := newFakeMeta(time.Hour)
	now := time.Now()
	p, _ := ParsePath("/gone.txt")
	parent, _ := ParsePath("/")
	fake.rows[p.String()] = MetaEntry{Size: 4, CachedAt: now}
	fake.stamps[parent.String()] = now

	stub := newStubS3()
	stub.put("gone.txt", 4)
	s, _ := newCachedTestS3(t, stub, fake)

	if err := s.Remove(t.Context(), p); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := fake.rows[p.String()]; ok {
		t.Fatal("removed row should be forgotten")
	}
	if _, ok := fake.stamps[parent.String()]; ok {
		t.Fatal("parent listing should be invalidated")
	}
}

func TestS3TrashForgetsAndRestoreInvalidates(t *testing.T) {
	fake := newFakeMeta(time.Hour)
	now := time.Now()
	p, _ := ParsePath("/trashme.txt")
	parent, _ := ParsePath("/")
	fake.rows[p.String()] = MetaEntry{Size: 4, CachedAt: now}
	fake.stamps[parent.String()] = now

	stub := newStubS3()
	stub.put("trashme.txt", 4)
	stub.put(".zefile-trash/", 0)
	s, _ := newCachedTestS3(t, stub, fake)

	id, err := s.Trash(t.Context(), p)
	if err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if _, ok := fake.rows[p.String()]; ok {
		t.Fatal("trashed row should be forgotten")
	}

	if err := s.Restore(t.Context(), id, p); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, ok := fake.stamps[parent.String()]; ok {
		t.Fatal("restore should invalidate the parent listing")
	}
	info, err := s.Stat(t.Context(), p)
	if err != nil {
		t.Fatalf("Stat after restore: %v", err)
	}
	if info.Size != 4 {
		t.Fatalf("size mismatch after restore: %+v", info)
	}
}
