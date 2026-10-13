package storage

import (
	"context"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// A capability URL must stay byte-identical across repeated requests
func TestS3PresignGetReusesURL(t *testing.T) {
	s := newTestS3(t, newStubS3(), nil)
	ctx := context.Background()
	target := MustParsePath("/photo.jpg")
	body := []byte("fake-jpeg-bytes")

	handle, err := s.BeginUpload(ctx, target, int64(len(body)))
	if err != nil {
		t.Fatalf("BeginUpload: %v", err)
	}
	urls, err := s.PresignParts(ctx, handle, []int{1})
	if err != nil {
		t.Fatalf("PresignParts: %v", err)
	}
	putPart(t, urls[0].URL, body)
	if err := s.CompleteUpload(ctx, handle, target); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}

	first, err := s.PresignGet(ctx, target, false, 0, "")
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	second, err := s.PresignGet(ctx, target, false, 0, "")
	if err != nil {
		t.Fatalf("PresignGet again: %v", err)
	}
	if first != second {
		t.Errorf("repeated PresignGet minted different URLs:\n%s\n%s", first, second)
	}

	// Anything that changes the URL's shape gets its own capability.
	other, err := s.PresignGet(ctx, target, false, 0, "attachment; filename=\"photo.jpg\"")
	if err != nil {
		t.Fatalf("PresignGet with disposition: %v", err)
	}
	if other == first {
		t.Error("a different disposition shared the plain URL")
	}
	headURL, err := s.PresignGet(ctx, target, true, 0, "")
	if err != nil {
		t.Fatalf("PresignGet head: %v", err)
	}
	if headURL == first {
		t.Error("HEAD shared the GET URL")
	}

	parsed, err := url.Parse(first)
	if err != nil {
		t.Fatalf("parse presigned URL: %v", err)
	}
	if got := parsed.Query().Get("response-cache-control"); got != presignCacheControl {
		t.Errorf("response-cache-control = %q, want %q", got, presignCacheControl)
	}
}

// The reuse table serves only what is still valid and cannot grow without
// limit on a longlived instance
func TestPresignCacheBounds(t *testing.T) {
	var c presignCache
	if _, ok := c.get("k"); ok {
		t.Fatal("empty cache reported a hit")
	}
	c.put("k", "u", time.Now().Add(time.Minute))
	if u, ok := c.get("k"); !ok || u != "u" {
		t.Errorf("get = %q, %v; want u, true", u, ok)
	}
	c.put("expired", "gone", time.Now().Add(-time.Second))
	if _, ok := c.get("expired"); ok {
		t.Error("expired entry was served")
	}
	for i := range presignMaxEntries + 10 {
		c.put("x"+strconv.Itoa(i), "u", time.Now().Add(time.Hour))
	}
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > presignMaxEntries {
		t.Errorf("cache holds %d entries, want at most %d", n, presignMaxEntries)
	}
}
