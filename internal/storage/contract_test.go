package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// Backend conformance: the same assertions run against every FS
// implementation. Cases that differ by nature (Local rename overwrites,
// S3 refuses) stay in the per-backend suites; everything here must hold
// everywhere.
type contractFixture struct {
	name string
	// open returns the backend and a seeder placing body at path.
	open func(t *testing.T) (FS, func(path string, body []byte))
}

func contractFixtures() []contractFixture {
	return []contractFixture{
		{
			name: "local",
			open: func(t *testing.T) (FS, func(string, []byte)) {
				t.Helper()
				fs := mustOpen(t, Config{Root: t.TempDir(), Reserve: 1})
				seed := func(path string, body []byte) {
					t.Helper()
					w, err := fs.Create(context.Background(), MustParsePath(path))
					if err != nil {
						t.Fatalf("seed Create: %v", err)
					}
					if _, err := w.Write(body); err != nil {
						t.Fatalf("seed Write: %v", err)
					}
					if err := w.Close(); err != nil {
						t.Fatalf("seed Close: %v", err)
					}
				}
				return fs, seed
			},
		},
		{
			name: "s3",
			open: func(t *testing.T) (FS, func(string, []byte)) {
				t.Helper()
				stub := newStubS3()
				seed := func(path string, body []byte) {
					t.Helper()
					key := MustParsePath(path).String()
					if len(key) > 0 && key[0] == '/' {
						key = key[1:]
					}
					stub.mu.Lock()
					stub.objects[key] = stubObject{
						size:    int64(len(body)),
						modTime: stubModTime,
						body:    append([]byte(nil), body...),
					}
					stub.mu.Unlock()
				}
				return newTestS3(t, stub, nil), seed
			},
		},
	}
}

func readAll(t *testing.T, fs FS, path string) []byte {
	t.Helper()
	f, err := fs.Open(context.Background(), MustParsePath(path))
	if err != nil {
		t.Fatalf("Open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return got
}

func TestContractRoundTrip(t *testing.T) {
	for _, fx := range contractFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			ctx := context.Background()
			fs, seed := fx.open(t)

			if err := fs.MkdirAll(ctx, MustParsePath("/docs")); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			dir, err := fs.Stat(ctx, MustParsePath("/docs"))
			if err != nil || !dir.IsDir {
				t.Fatalf("Stat dir = %+v, %v; want a directory", dir, err)
			}

			body := []byte("contract payload")
			seed("/docs/file.txt", body)

			info, err := fs.Stat(ctx, MustParsePath("/docs/file.txt"))
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if info.Size != int64(len(body)) || info.IsDir || info.Name != "file.txt" {
				t.Fatalf("Stat = %+v, want the seeded file", info)
			}
			if got := readAll(t, fs, "/docs/file.txt"); !bytes.Equal(got, body) {
				t.Fatalf("read %q, want %q", got, body)
			}

			entries, err := fs.List(ctx, MustParsePath("/docs"))
			if err != nil || len(entries) != 1 {
				t.Fatalf("List = %+v, %v; want one entry", entries, err)
			}

			if err := fs.Move(ctx, MustParsePath("/docs/file.txt"), MustParsePath("/docs/moved.txt")); err != nil {
				t.Fatalf("Move: %v", err)
			}
			if _, err := fs.Stat(ctx, MustParsePath("/docs/file.txt")); !errors.Is(err, ErrNotExist) {
				t.Fatalf("old path = %v, want ErrNotExist", err)
			}
			if got := readAll(t, fs, "/docs/moved.txt"); !bytes.Equal(got, body) {
				t.Fatalf("moved read %q, want %q", got, body)
			}

			if err := fs.Copy(ctx, MustParsePath("/docs/moved.txt"), MustParsePath("/docs/copy.txt")); err != nil {
				t.Fatalf("Copy: %v", err)
			}
			if got := readAll(t, fs, "/docs/copy.txt"); !bytes.Equal(got, body) {
				t.Fatalf("copied read %q, want %q", got, body)
			}

			if err := fs.Remove(ctx, MustParsePath("/docs/copy.txt")); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			if err := fs.RemoveAll(ctx, MustParsePath("/docs")); err != nil {
				t.Fatalf("RemoveAll: %v", err)
			}
			if _, err := fs.Stat(ctx, MustParsePath("/docs")); !errors.Is(err, ErrNotExist) {
				t.Fatalf("tree = %v, want ErrNotExist", err)
			}
		})
	}
}

func TestContractErrors(t *testing.T) {
	for _, fx := range contractFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			ctx := context.Background()
			fs, seed := fx.open(t)

			if _, err := fs.Stat(ctx, MustParsePath("/missing")); !errors.Is(err, ErrNotExist) {
				t.Errorf("Stat missing = %v, want ErrNotExist", err)
			}
			if _, err := fs.Open(ctx, MustParsePath("/missing")); !errors.Is(err, ErrNotExist) {
				t.Errorf("Open missing = %v, want ErrNotExist", err)
			}
			if _, err := fs.Open(ctx, MustParsePath("/")); !errors.Is(err, ErrIsDir) {
				t.Errorf("Open root = %v, want ErrIsDir", err)
			}
			if err := fs.Remove(ctx, MustParsePath("/missing")); !errors.Is(err, ErrNotExist) {
				t.Errorf("Remove missing = %v, want ErrNotExist", err)
			}

			if err := fs.MkdirAll(ctx, MustParsePath("/full")); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			seed("/full/a.txt", []byte("a"))
			if err := fs.Remove(ctx, MustParsePath("/full")); !errors.Is(err, ErrNotEmpty) {
				t.Errorf("Remove non-empty = %v, want ErrNotEmpty", err)
			}
			seed("/full/b.txt", []byte("b"))
			if err := fs.Copy(ctx, MustParsePath("/full/a.txt"), MustParsePath("/full/b.txt")); !errors.Is(err, ErrExist) {
				t.Errorf("Copy onto existing = %v, want ErrExist", err)
			}
		})
	}
}

func TestContractTrash(t *testing.T) {
	for _, fx := range contractFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			ctx := context.Background()
			fs, seed := fx.open(t)
			trasher, ok := fs.(Trasher)
			if !ok {
				t.Fatalf("%T has no trash", fs)
			}

			body := []byte("trash me")
			seed("/gone.txt", body)

			id, err := trasher.Trash(ctx, MustParsePath("/gone.txt"))
			if err != nil {
				t.Fatalf("Trash: %v", err)
			}
			if _, err := fs.Stat(ctx, MustParsePath("/gone.txt")); !errors.Is(err, ErrNotExist) {
				t.Fatalf("trashed source = %v, want ErrNotExist", err)
			}
			if err := trasher.Restore(ctx, id, MustParsePath("/back.txt")); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if got := readAll(t, fs, "/back.txt"); !bytes.Equal(got, body) {
				t.Fatalf("restored read %q, want %q", got, body)
			}

			id, err = trasher.Trash(ctx, MustParsePath("/back.txt"))
			if err != nil {
				t.Fatalf("Trash again: %v", err)
			}
			if err := trasher.PurgeTrash(ctx, id); err != nil {
				t.Fatalf("PurgeTrash: %v", err)
			}
			if _, err := trasher.TrashStat(ctx, id); !errors.Is(err, ErrNotExist) {
				t.Fatalf("purged stat = %v, want ErrNotExist", err)
			}
		})
	}
}

func TestContractGuard(t *testing.T) {
	// A denied path is absence-shaped everywhere: callers must not learn
	// whether it exists from which error comes back.
	openDenied := func(t *testing.T, fs FS) {
		t.Helper()
		ctx := context.Background()
		if _, err := fs.Stat(ctx, MustParsePath("/secret.txt")); !errors.Is(err, ErrPermission) {
			t.Errorf("Stat denied = %v, want ErrPermission", err)
		}
		if _, err := fs.Open(ctx, MustParsePath("/secret.txt")); !errors.Is(err, ErrPermission) {
			t.Errorf("Open denied = %v, want ErrPermission", err)
		}
	}

	t.Run("local", func(t *testing.T) {
		fs := mustOpen(t, Config{Root: t.TempDir(), Reserve: 1, Guard: denyGuard{deny: "/secret.txt"}})
		openDenied(t, fs)
	})
	t.Run("s3", func(t *testing.T) {
		fs := newTestS3(t, newStubS3(), func(cfg *S3Config) {
			cfg.Guard = denyGuard{deny: "/secret.txt"}
		})
		openDenied(t, fs)
	})
}
