package storage

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func collectWalk(t *testing.T, w Walker) map[string]int64 {
	t.Helper()
	got := map[string]int64{}
	if err := w.Walk(context.Background(), func(path string, size int64) error {
		got[path] = size
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return got
}

func writeSized(t *testing.T, p string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLocalWalkSkipsReserved(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeSized(t, filepath.Join(root, "a.bin"), 10)
	writeSized(t, filepath.Join(root, "sub", "b.bin"), 20)
	writeSized(t, filepath.Join(root, ".zefile-trash", "gone.bin"), 1000)
	writeSized(t, filepath.Join(root, ".zefile-thumbs", "x.jpg"), 1000)
	writeSized(t, filepath.Join(root, ".zefile-uploads", "part"), 1000)

	fs := mustOpen(t, Config{Root: root, Reserve: 1})
	got := collectWalk(t, fs)

	want := map[string]int64{"/a.bin": 10, "/sub/b.bin": 20}
	if len(got) != len(want) {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf("Walk = %v, want %v", keys, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Walk[%q] = %d, want %d", k, got[k], v)
		}
	}
}

func TestS3WalkSkipsMarkersAndReserved(t *testing.T) {
	t.Parallel()

	stub := newStubS3()
	stub.put("docs/", 0)                    // directory marker
	stub.put("docs/a.bin", 10)              // live file
	stub.put("docs/sub/b.bin", 20)          // nested live file
	stub.put(".zefile-trash/gone.bin", 999) // reserved
	stub.put(".zefile-thumbs/x.jpg", 999)   // reserved
	s := newTestS3(t, stub, nil)

	got := collectWalk(t, s)
	want := map[string]int64{"/docs/a.bin": 10, "/docs/sub/b.bin": 20}
	if len(got) != len(want) {
		t.Fatalf("Walk = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Walk[%q] = %d, want %d", k, got[k], v)
		}
	}
}
