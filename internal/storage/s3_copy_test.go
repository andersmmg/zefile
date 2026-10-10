package storage

import (
	"context"
	"errors"
	"testing"
)

// Copy, move and delete against the stub: what matters is which keys
// exist afterwards and which refusals come back.

func TestS3CopyFile(t *testing.T) {
	stub := newStubS3()
	stub.put("report.pdf", 5)
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	if err := s.Copy(ctx, MustParsePath("/report.pdf"), MustParsePath("/copie.pdf")); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	got, err := s.Stat(ctx, MustParsePath("/copie.pdf"))
	if err != nil {
		t.Fatalf("Stat(dst): %v", err)
	}
	if got.IsDir || got.Size != 5 {
		t.Errorf("Stat(dst) = %+v, want a 5-byte file", got)
	}
	// The source is untouched: a copy duplicates, it does not move.
	src, err := s.Stat(ctx, MustParsePath("/report.pdf"))
	if err != nil || src.Size != 5 {
		t.Errorf("Stat(src) = %+v, %v; want the 5-byte original", src, err)
	}
}

func TestS3CopyRefusals(t *testing.T) {
	stub := newStubS3()
	stub.put("a.txt", 1)
	stub.put("b.txt", 2)
	stub.put("big.iso", MaxSyncCopyBytes+1) // size only — refused before any copy
	stub.put("docs/", 0)
	stub.put("docs/leaf.txt", 3)
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	cases := []struct {
		name string
		from string
		to   string
		want error
	}{
		{"overwrite refused", "/a.txt", "/b.txt", ErrExist},
		{"missing source", "/phantom.txt", "/x.txt", ErrNotExist},
		{"directory needs the queue", "/docs", "/docs-copy", ErrIsDir},
		{"oversize needs the queue", "/big.iso", "/big-copy.iso", ErrTooLarge},
		{"missing parent", "/a.txt", "/nodir/x.txt", ErrNotExist},
		{"root source", "/", "/x.txt", ErrInvalidPath},
		{"root destination", "/a.txt", "/", ErrInvalidPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Copy(ctx, MustParsePath(tc.from), MustParsePath(tc.to))
			if !errors.Is(err, tc.want) {
				t.Errorf("Copy(%s, %s) = %v, want %v", tc.from, tc.to, err, tc.want)
			}
		})
	}
	if stub.has("x.txt") || stub.has("docs-copy") || stub.has("big-copy.iso") {
		t.Error("a refused copy left a destination behind")
	}

	// A denied source reports permission, not absence.
	guarded := newTestS3(t, stub, func(cfg *S3Config) { cfg.Guard = denyGuard{deny: "/a.txt"} })
	if err := guarded.Copy(ctx, MustParsePath("/a.txt"), MustParsePath("/c.txt")); !errors.Is(err, ErrPermission) {
		t.Errorf("Copy of a denied source = %v, want ErrPermission", err)
	}
}

func TestS3MoveFile(t *testing.T) {
	stub := newStubS3()
	stub.put("report.pdf", 5)
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	if err := s.Move(ctx, MustParsePath("/report.pdf"), MustParsePath("/archive/report.pdf")); err == nil {
		t.Error("Move into a missing parent succeeded")
	} else if !errors.Is(err, ErrNotExist) {
		t.Errorf("Move into a missing parent = %v, want ErrNotExist", err)
	}

	if err := s.Mkdir(ctx, MustParsePath("/archive")); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := s.Move(ctx, MustParsePath("/report.pdf"), MustParsePath("/archive/report.pdf")); err != nil {
		t.Fatalf("Move: %v", err)
	}
	got, err := s.Stat(ctx, MustParsePath("/archive/report.pdf"))
	if err != nil || got.IsDir || got.Size != 5 {
		t.Errorf("Stat(dst) = %+v, %v; want the moved 5-byte file", got, err)
	}
	if _, err := s.Stat(ctx, MustParsePath("/report.pdf")); !errors.Is(err, ErrNotExist) {
		t.Errorf("Stat(src) = %v, want ErrNotExist", err)
	}
}

func TestS3MoveRefusals(t *testing.T) {
	stub := newStubS3()
	stub.put("a.txt", 1)
	stub.put("b.txt", 2)
	stub.put("docs/", 0)
	stub.put("docs/leaf.txt", 3)
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	if err := s.Move(ctx, MustParsePath("/a.txt"), MustParsePath("/b.txt")); !errors.Is(err, ErrExist) {
		t.Errorf("Move onto an existing file = %v, want ErrExist", err)
	}
	if err := s.Move(ctx, MustParsePath("/docs"), MustParsePath("/docs/child")); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("Move into itself = %v, want ErrInvalidPath", err)
	}
	if err := s.Move(ctx, MustParsePath("/phantom.txt"), MustParsePath("/x.txt")); !errors.Is(err, ErrNotExist) {
		t.Errorf("Move of a missing file = %v, want ErrNotExist", err)
	}
	if err := s.Move(ctx, MustParsePath("/"), MustParsePath("/x")); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("Move of the root = %v, want ErrInvalidPath", err)
	}

	// Renaming onto itself is refused, not a silent no-op.
	if err := s.Move(ctx, MustParsePath("/a.txt"), MustParsePath("/a.txt")); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("Move onto itself = %v, want ErrInvalidPath", err)
	}

	guarded := newTestS3(t, stub, func(cfg *S3Config) { cfg.Guard = denyGuard{deny: "/a.txt"} })
	if err := guarded.Move(ctx, MustParsePath("/a.txt"), MustParsePath("/c.txt")); !errors.Is(err, ErrPermission) {
		t.Errorf("Move of a denied source = %v, want ErrPermission", err)
	}
}

func TestS3MoveDir(t *testing.T) {
	stub := newStubS3("docs/")
	stub.put("docs/", 0)
	stub.put("docs/leaf.txt", 3)
	stub.put("docs/sub/", 0)
	stub.put("docs/sub/deep.txt", 7)
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	if err := s.Move(ctx, MustParsePath("/docs"), MustParsePath("/archive")); err != nil {
		t.Fatalf("Move dir: %v", err)
	}
	for _, key := range []string{"archive/", "archive/leaf.txt", "archive/sub/", "archive/sub/deep.txt"} {
		if !stub.has(key) {
			t.Errorf("destination %#q missing after move", key)
		}
	}
	for _, key := range []string{"docs/", "docs/leaf.txt", "docs/sub/", "docs/sub/deep.txt"} {
		if stub.has(key) {
			t.Errorf("source %#q still present after move", key)
		}
	}
	deep, err := s.Stat(ctx, MustParsePath("/archive/sub/deep.txt"))
	if err != nil || deep.Size != 7 {
		t.Errorf("Stat(moved nested file) = %+v, %v; want 7 bytes", deep, err)
	}
}

func TestS3CopyTree(t *testing.T) {
	stub := newStubS3("docs/")
	stub.put("docs/", 0)
	stub.put("docs/leaf.txt", 3)
	stub.put("docs/sub/deep.txt", 7)
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	var done, total int64
	calls := 0
	if err := s.CopyTree(ctx, MustParsePath("/docs"), MustParsePath("/backup"), func(d, tot int64) {
		done, total, calls = d, tot, calls+1
	}); err != nil {
		t.Fatalf("CopyTree: %v", err)
	}
	if total != 10 || done != 10 {
		t.Errorf("progress ended at %d/%d, want 10/10", done, total)
	}
	if calls == 0 {
		t.Error("progress was never reported")
	}
	for _, key := range []string{"backup/", "backup/leaf.txt", "backup/sub/deep.txt"} {
		if !stub.has(key) {
			t.Errorf("destination %#q missing after CopyTree", key)
		}
	}
	// The source is untouched.
	if !stub.has("docs/leaf.txt") {
		t.Error("CopyTree removed the source")
	}

	// A file copies through the tree path too, with no size ceiling here.
	if err := s.CopyTree(ctx, MustParsePath("/docs/leaf.txt"), MustParsePath("/leaf-copy.txt"), nil); err != nil {
		t.Errorf("CopyTree of a file: %v", err)
	}

	if err := s.CopyTree(ctx, MustParsePath("/docs"), MustParsePath("/backup"), nil); !errors.Is(err, ErrExist) {
		t.Errorf("CopyTree onto an existing path = %v, want ErrExist", err)
	}
	if err := s.CopyTree(ctx, MustParsePath("/phantom"), MustParsePath("/x"), nil); !errors.Is(err, ErrNotExist) {
		t.Errorf("CopyTree of a missing path = %v, want ErrNotExist", err)
	}
}

func TestS3Remove(t *testing.T) {
	stub := newStubS3("docs/", "empty/")
	stub.put("a.txt", 1)
	stub.put("docs/", 0)
	stub.put("docs/leaf.txt", 3)
	stub.put("empty/", 0)
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	if err := s.Remove(ctx, MustParsePath("/a.txt")); err != nil {
		t.Fatalf("Remove file: %v", err)
	}
	if stub.has("a.txt") {
		t.Error("file still present after Remove")
	}
	if err := s.Remove(ctx, MustParsePath("/a.txt")); !errors.Is(err, ErrNotExist) {
		t.Errorf("Remove of a missing file = %v, want ErrNotExist", err)
	}
	if err := s.Remove(ctx, MustParsePath("/docs")); !errors.Is(err, ErrNotEmpty) {
		t.Errorf("Remove of a non-empty directory = %v, want ErrNotEmpty", err)
	}
	if !stub.has("docs/leaf.txt") {
		t.Error("a refused Remove deleted the contents")
	}
	if err := s.Remove(ctx, MustParsePath("/empty")); err != nil {
		t.Fatalf("Remove of an empty directory: %v", err)
	}
	if stub.has("empty/") {
		t.Error("empty directory marker still present after Remove")
	}
	if err := s.Remove(ctx, MustParsePath("/")); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("Remove of the root = %v, want ErrInvalidPath", err)
	}
}

func TestS3RemoveAll(t *testing.T) {
	stub := newStubS3("docs/")
	stub.put("docs/", 0)
	stub.put("docs/leaf.txt", 3)
	stub.put("docs/sub/deep.txt", 7)
	stub.put("keep.txt", 1)
	stub.put("docs-draft.txt", 9) // shares the spelling, not the directory
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	if err := s.RemoveAll(ctx, MustParsePath("/docs")); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	for _, key := range []string{"docs/", "docs/leaf.txt", "docs/sub/deep.txt"} {
		if stub.has(key) {
			t.Errorf("%#q still present after RemoveAll", key)
		}
	}
	for _, key := range []string{"keep.txt", "docs-draft.txt"} {
		if !stub.has(key) {
			t.Errorf("%#q wrongly removed by RemoveAll", key)
		}
	}
	// Idempotent, like os.RemoveAll.
	if err := s.RemoveAll(ctx, MustParsePath("/docs")); err != nil {
		t.Errorf("RemoveAll of a missing path = %v, want nil", err)
	}
	if err := s.RemoveAll(ctx, MustParsePath("/")); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("RemoveAll of the root = %v, want ErrInvalidPath", err)
	}
}
