package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// seedBody stores an object with deterministic bytes in the stub.
func seedBody(stub *stubS3, key string, size int) []byte {
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i * 31 % 251)
	}
	stub.mu.Lock()
	stub.objects[key] = stubObject{size: int64(size), modTime: stubModTime, body: body}
	stub.mu.Unlock()
	return body
}

func TestS3OpenRead(t *testing.T) {
	stub := newStubS3()
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	want := seedBody(stub, "docs/report.bin", 3000)
	p := MustParsePath("/docs/report.bin")

	f, err := s.Open(ctx, p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = f.Close() }()

	if fi, err := f.Stat(); err != nil || fi.Size() != int64(len(want)) {
		t.Fatalf("Stat = %+v, %v; want size %d", fi, err, len(want))
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes, want %d identical", len(got), len(want))
	}
	if _, err := f.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read past end = %v, want EOF", err)
	}
}

func TestS3OpenSeek(t *testing.T) {
	stub := newStubS3()
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	want := seedBody(stub, "seek.bin", 1000)
	p := MustParsePath("/seek.bin")

	f, err := s.Open(ctx, p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = f.Close() }()

	// Skip ahead, read a window, seek back and reread it.
	if _, err := f.Seek(700, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	window := make([]byte, 100)
	if _, err := io.ReadFull(f, window); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(window, want[700:800]) {
		t.Fatalf("window mismatch after seek")
	}
	if _, err := f.Seek(-100, io.SeekCurrent); err != nil {
		t.Fatalf("SeekCurrent: %v", err)
	}
	again := make([]byte, 100)
	if _, err := io.ReadFull(f, again); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if !bytes.Equal(again, window) {
		t.Fatalf("reread mismatch")
	}

	// From the end, and past the end.
	if off, err := f.Seek(-10, io.SeekEnd); err != nil || off != 990 {
		t.Fatalf("SeekEnd = %d, %v; want 990", off, err)
	}
	tail := make([]byte, 10)
	if _, err := io.ReadFull(f, tail); err != nil {
		t.Fatalf("tail: %v", err)
	}
	if !bytes.Equal(tail, want[990:]) {
		t.Fatalf("tail mismatch")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	head := make([]byte, 5)
	if _, err := io.ReadFull(f, head); err != nil {
		t.Fatalf("head: %v", err)
	}
	if !bytes.Equal(head, want[:5]) {
		t.Fatalf("head mismatch after rewind")
	}
	if _, err := f.Seek(-1, io.SeekStart); err == nil {
		t.Fatalf("negative seek succeeded, want an error")
	}
}

func TestS3OpenRefusals(t *testing.T) {
	stub := newStubS3()
	seedBody(stub, "file.bin", 10)
	stub.put("dir/", 0)
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	if _, err := s.Open(ctx, MustParsePath("/missing.bin")); !errors.Is(err, ErrNotExist) {
		t.Errorf("missing = %v, want ErrNotExist", err)
	}
	if _, err := s.Open(ctx, MustParsePath("/dir")); !errors.Is(err, ErrIsDir) {
		t.Errorf("dir = %v, want ErrIsDir", err)
	}
	if _, err := s.Open(ctx, MustParsePath("/")); !errors.Is(err, ErrIsDir) {
		t.Errorf("root = %v, want ErrIsDir", err)
	}

	denied := newTestS3(t, stub, func(cfg *S3Config) {
		cfg.Guard = denyGuard{deny: "/file.bin"}
	})
	if _, err := denied.Open(ctx, MustParsePath("/file.bin")); !errors.Is(err, ErrPermission) {
		t.Errorf("denied = %v, want ErrPermission", err)
	}
}
