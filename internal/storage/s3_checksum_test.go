package storage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
)

func TestS3ChecksumRoundTrip(t *testing.T) {
	stub := newStubS3()
	s := newTestS3(t, stub, nil)
	ctx := context.Background()
	body := []byte("the quick brown fox jumps over the lazy dog")
	sum := sha256.Sum256(body)

	stub.mu.Lock()
	stub.objects["docs/report.pdf"] = stubObject{
		size:           int64(len(body)),
		modTime:        stubModTime,
		body:           body,
		checksumSHA256: base64.StdEncoding.EncodeToString(sum[:]),
	}
	stub.mu.Unlock()

	got, err := s.ObjectChecksum(ctx, MustParsePath("/docs/report.pdf"))
	if err != nil {
		t.Fatalf("ObjectChecksum: %v", err)
	}
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("ObjectChecksum = %q, want %q", got, want)
	}
}

func TestS3ChecksumEmptyFile(t *testing.T) {
	s := newTestS3(t, newStubS3(), nil)
	ctx := context.Background()
	target := MustParsePath("/empty.txt")

	handle, err := s.BeginUpload(ctx, target, 0)
	if err != nil {
		t.Fatalf("BeginUpload: %v", err)
	}
	if err := s.CompleteUpload(ctx, handle, target); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}

	want := sha256.Sum256(nil)
	got, err := s.ObjectChecksum(ctx, target)
	if err != nil {
		t.Fatalf("ObjectChecksum: %v", err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Errorf("ObjectChecksum = %q, want the SHA-256 of the empty file", got)
	}
}

func TestS3ChecksumRefusals(t *testing.T) {
	stub := newStubS3()
	stub.put("plain.bin", 4)
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	if _, err := s.ObjectChecksum(ctx, MustParsePath("/plain.bin")); !errors.Is(err, ErrNotSupported) {
		t.Errorf("checksum of an object without one = %v, want ErrNotSupported", err)
	}

	stub.mu.Lock()
	unc := stub.objects["plain.bin"]
	unc.checksumSHA256 = "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXo-3"
	stub.objects["plain.bin"] = unc
	stub.mu.Unlock()

	if _, err := s.ObjectChecksum(ctx, MustParsePath("/plain.bin")); !errors.Is(err, ErrNotSupported) {
		t.Errorf("composite checksum = %v, want ErrNotSupported", err)
	}

	if _, err := s.ObjectChecksum(ctx, MustParsePath("/missing.bin")); !errors.Is(err, ErrNotExist) {
		t.Errorf("checksum of a missing file = %v, want ErrNotExist", err)
	}
}
