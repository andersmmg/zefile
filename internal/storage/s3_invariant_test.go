package storage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

// The acceptance test for the storage invariant: during a full
// upload+download cycle the server handles no object bytes. Part uploads
// and downloads travel presigned browser↔bucket legs; every backend-issued
// request is metadata (create, listparts, complete, head, list).
func TestS3UploadDownloadMovesNoBytes(t *testing.T) {
	stub := newStubS3()
	var violations []string
	stub.violations = &violations
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	if err := s.MkdirAll(ctx, MustParsePath("/cycle")); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	target := MustParsePath("/cycle/file.bin")
	body := bytes.Repeat([]byte("payload "), 1<<16) // 512 KiB

	handle, err := s.BeginUpload(ctx, target, int64(len(body)))
	if err != nil {
		t.Fatalf("BeginUpload: %v", err)
	}
	urls, err := s.PresignParts(ctx, handle, []int{1, 2})
	if err != nil {
		t.Fatalf("PresignParts: %v", err)
	}
	half := len(body) / 2
	for i, u := range urls {
		chunk := body[:half]
		if i == 1 {
			chunk = body[half:]
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.URL, bytes.NewReader(chunk))
		if err != nil {
			t.Fatalf("part request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("part PUT: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("part PUT = %d, want 200", resp.StatusCode)
		}
	}
	if err := s.CompleteUpload(ctx, handle, target); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}

	link, err := s.PresignGet(ctx, target, false, time.Minute, "")
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	resp, err := http.Get(link)
	if err != nil {
		t.Fatalf("download GET: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("download read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("downloaded %d bytes, want %d identical", len(got), len(body))
	}

	if len(violations) > 0 {
		t.Fatalf("backend handled object bytes: %v", violations)
	}
}
