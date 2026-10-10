package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The write path against the stub: a full multipart round trip goes through
// presigned URLs the way a browser would, so these tests exercise the same
// wire the client speaks — not just the SDK calls.

// putPart PUTs bytes to a presigned part URL, the way the web client does.
func putPart(t *testing.T, url string, body []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build part request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT part: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("PUT part: status %d", resp.StatusCode)
	}
}

func TestPartSizeFor(t *testing.T) {
	const part = 16 << 20
	for _, tc := range []struct {
		name       string
		configured int64
		size       int64
		want       int64
	}{
		{"small file keeps configured", part, 1 << 20, part},
		{"empty file keeps configured", part, 0, part},
		{"huge file grows to fit 10000 parts", part, 4 << 42, 1759218605}, // ceil(4TiB / 10000)
		{"larger configured wins", 64 << 20, 1 << 20, 64 << 20},
	} {
		if got := PartSizeFor(tc.configured, tc.size); got != tc.want {
			t.Errorf("%s: PartSizeFor = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestS3DirectUploadRoundTrip(t *testing.T) {
	stub := newStubS3()
	s := newTestS3(t, stub, func(cfg *S3Config) { cfg.PartSize = 5 << 20 })
	ctx := context.Background()

	target := MustParsePath("/docs/report.bin")
	body := bytes.Repeat([]byte("0123456789abcdef"), 1<<20) // 16 MiB

	// The parent must exist first: an upload lands in a folder, never
	// conjures one.
	if err := s.MkdirAll(ctx, MustParsePath("/docs")); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	handle, err := s.BeginUpload(ctx, target, int64(len(body)))
	if err != nil {
		t.Fatalf("BeginUpload: %v", err)
	}
	if handle.PartSize != 5<<20 {
		t.Fatalf("PartSize = %d, want %d", handle.PartSize, 5<<20)
	}

	// Four parts: 5 + 5 + 5 + 1 MiB.
	numbers := []int{1, 2, 3, 4}
	urls, err := s.PresignParts(ctx, handle, numbers)
	if err != nil {
		t.Fatalf("PresignParts: %v", err)
	}
	if len(urls) != len(numbers) {
		t.Fatalf("PresignParts returned %d URLs, want %d", len(urls), len(numbers))
	}
	for i, u := range urls {
		start := int64(i) * (5 << 20)
		end := min(start+(5<<20), int64(len(body)))
		putPart(t, u.URL, body[start:end])
		if u.Number != numbers[i] {
			t.Fatalf("URL %d addresses part %d", i, u.Number)
		}
	}

	if got, err := s.CommittedBytes(ctx, handle); err != nil || got != int64(len(body)) {
		t.Fatalf("CommittedBytes = %d, %v; want %d", got, err, len(body))
	}

	if err := s.CompleteUpload(ctx, handle, target); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}

	info, err := s.Stat(ctx, target)
	if err != nil {
		t.Fatalf("Stat after complete: %v", err)
	}
	if info.Size != int64(len(body)) || info.IsDir {
		t.Fatalf("Stat = %+v, want a %d-byte file", info, len(body))
	}

	// The bytes survived the trip whole.
	stub.mu.Lock()
	got := stub.objects["docs/report.bin"].body
	stub.mu.Unlock()
	if !bytes.Equal(got, body) {
		t.Fatal("reassembled object differs from what was sent")
	}
}

func TestS3DirectUploadRefusals(t *testing.T) {
	stub := newStubS3("taken.txt")
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	if _, err := s.BeginUpload(ctx, MustParsePath("/taken.txt"), 10); !errors.Is(err, ErrExist) {
		t.Errorf("BeginUpload over a file = %v, want ErrExist", err)
	}
	if _, err := s.BeginUpload(ctx, MustParsePath("/no/such/dir/file"), 10); !errors.Is(err, ErrNotExist) {
		t.Errorf("BeginUpload without a parent = %v, want ErrNotExist", err)
	}
	if _, err := s.BeginUpload(ctx, MustParsePath("/neg"), -1); err == nil {
		t.Error("BeginUpload with a negative size succeeded")
	}

	// Completing with nothing uploaded refuses rather than publishing an
	// empty shell the client never sent.
	handle, err := s.BeginUpload(ctx, MustParsePath("/empty-shell"), 100)
	if err != nil {
		t.Fatalf("BeginUpload: %v", err)
	}
	if err := s.CompleteUpload(ctx, handle, MustParsePath("/empty-shell")); err == nil {
		t.Error("CompleteUpload with no parts succeeded")
	}

	// A handle is bound to its destination: completing it elsewhere is a
	// programming error and fails loudly.
	if err := s.CompleteUpload(ctx, handle, MustParsePath("/elsewhere")); err == nil {
		t.Error("CompleteUpload at another path succeeded")
	}

	// Abort is idempotent, and afterwards the bytes are reported gone.
	if err := s.AbortUpload(ctx, handle); err != nil {
		t.Fatalf("AbortUpload: %v", err)
	}
	if err := s.AbortUpload(ctx, handle); err != nil {
		t.Errorf("second AbortUpload: %v", err)
	}
	if _, err := s.CommittedBytes(ctx, handle); !errors.Is(err, ErrNotExist) {
		t.Errorf("CommittedBytes after abort = %v, want ErrNotExist", err)
	}
}

func TestS3EmptyFileUpload(t *testing.T) {
	s := newTestS3(t, newStubS3(), nil)
	ctx := context.Background()
	target := MustParsePath("/nothing.txt")

	handle, err := s.BeginUpload(ctx, target, 0)
	if err != nil {
		t.Fatalf("BeginUpload: %v", err)
	}
	if _, err := s.PresignParts(ctx, handle, []int{1}); err == nil {
		t.Error("PresignParts for an empty file succeeded")
	}
	if err := s.CompleteUpload(ctx, handle, target); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	info, err := s.Stat(ctx, target)
	if err != nil || info.Size != 0 || info.IsDir {
		t.Fatalf("Stat = %+v, %v; want an empty file", info, err)
	}
}

func TestS3PresignGet(t *testing.T) {
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

	// A fetched presigned URL carries the bytes and the disposition override.
	fetched, err := s.PresignGet(ctx, target, false, 0, "inline; filename*=UTF-8''photo.jpg")
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	resp, err := http.Get(fetched)
	if err != nil {
		t.Fatalf("GET presigned URL: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Fatal("presigned GET returned other bytes than the object holds")
	}
	if disp := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(disp, "inline") {
		t.Errorf("Content-Disposition = %q, want the inline override", disp)
	}

	// HEAD variant answers metadata without a body.
	headURL, err := s.PresignGet(ctx, target, true, 0, "")
	if err != nil {
		t.Fatalf("PresignGet head: %v", err)
	}
	head, err := http.Head(headURL)
	if err != nil {
		t.Fatalf("HEAD presigned URL: %v", err)
	}
	head.Body.Close()
	if head.StatusCode != http.StatusOK || head.ContentLength != int64(len(body)) {
		t.Errorf("HEAD = %d len %d, want 200 len %d", head.StatusCode, head.ContentLength, len(body))
	}

	if _, err := s.PresignGet(ctx, MustParsePath("/missing"), false, 0, ""); !errors.Is(err, ErrNotExist) {
		t.Errorf("PresignGet of a missing file = %v, want ErrNotExist", err)
	}
}

func TestS3PresignUsesPublicEndpoint(t *testing.T) {
	s := newTestS3(t, newStubS3(), func(cfg *S3Config) {
		cfg.PublicEndpoint = "http://public.example:8333"
	})
	ctx := context.Background()

	handle, err := s.BeginUpload(ctx, MustParsePath("/f"), 10)
	if err != nil {
		t.Fatalf("BeginUpload: %v", err)
	}
	urls, err := s.PresignParts(ctx, handle, []int{1})
	if err != nil {
		t.Fatalf("PresignParts: %v", err)
	}
	if !strings.HasPrefix(urls[0].URL, "http://public.example:8333/") {
		t.Errorf("part URL = %q, want the public host", urls[0].URL)
	}
	got, err := s.PresignGet(ctx, MustParsePath("/f"), false, 0, "")
	if err == nil || !errors.Is(err, ErrNotExist) {
		t.Fatalf("PresignGet should fail on the missing file first, got %q, %v", got, err)
	}
}

func TestS3TrashRoundTrip(t *testing.T) {
	stub := newStubS3()
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	// A file with content and a directory with contents go in.
	fileBody := []byte("do not lose me")
	fh, err := s.BeginUpload(ctx, MustParsePath("/keep.txt"), int64(len(fileBody)))
	if err != nil {
		t.Fatalf("BeginUpload: %v", err)
	}
	pu, err := s.PresignParts(ctx, fh, []int{1})
	if err != nil {
		t.Fatalf("PresignParts: %v", err)
	}
	putPart(t, pu[0].URL, fileBody)
	if err := s.CompleteUpload(ctx, fh, MustParsePath("/keep.txt")); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if err := s.MkdirAll(ctx, MustParsePath("/box/inner")); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dh, err := s.BeginUpload(ctx, MustParsePath("/box/note.txt"), 3)
	if err != nil {
		t.Fatalf("BeginUpload: %v", err)
	}
	du, err := s.PresignParts(ctx, dh, []int{1})
	if err != nil {
		t.Fatalf("PresignParts: %v", err)
	}
	putPart(t, du[0].URL, []byte("hi!"))
	if err := s.CompleteUpload(ctx, dh, MustParsePath("/box/note.txt")); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}

	// Trashing a file hides it and reports it back faithfully.
	fileID, err := s.Trash(ctx, MustParsePath("/keep.txt"))
	if err != nil {
		t.Fatalf("Trash file: %v", err)
	}
	if _, err := s.Stat(ctx, MustParsePath("/keep.txt")); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Stat after trash = %v, want ErrNotExist", err)
	}
	info, err := s.TrashStat(ctx, fileID)
	if err != nil || info.IsDir() || info.Size() != int64(len(fileBody)) {
		t.Fatalf("TrashStat = %+v, %v; want the %d-byte file", info, err, len(fileBody))
	}

	// Trashing a directory moves the whole subtree, markers included.
	dirID, err := s.Trash(ctx, MustParsePath("/box"))
	if err != nil {
		t.Fatalf("Trash dir: %v", err)
	}
	if _, err := s.Stat(ctx, MustParsePath("/box")); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Stat after dir trash = %v, want ErrNotExist", err)
	}
	dirInfo, err := s.TrashStat(ctx, dirID)
	if err != nil || !dirInfo.IsDir() {
		t.Fatalf("TrashStat dir = %+v, %v; want a directory", dirInfo, err)
	}

	// Restoring puts the bytes back where they were.
	if err := s.Restore(ctx, fileID, MustParsePath("/keep.txt")); err != nil {
		t.Fatalf("Restore file: %v", err)
	}
	stub.mu.Lock()
	restored := stub.objects["keep.txt"].body
	stub.mu.Unlock()
	if !bytes.Equal(restored, fileBody) {
		t.Fatal("restored file content differs")
	}
	if err := s.Restore(ctx, dirID, MustParsePath("/box")); err != nil {
		t.Fatalf("Restore dir: %v", err)
	}
	if _, err := s.Stat(ctx, MustParsePath("/box/note.txt")); err != nil {
		t.Fatalf("Stat of restored nested file: %v", err)
	}

	// Restoring over an existing file refuses.
	againID, err := s.Trash(ctx, MustParsePath("/keep.txt"))
	if err != nil {
		t.Fatalf("Trash again: %v", err)
	}
	nh, err := s.BeginUpload(ctx, MustParsePath("/keep.txt"), 1)
	if err != nil {
		t.Fatalf("BeginUpload: %v", err)
	}
	nu, err := s.PresignParts(ctx, nh, []int{1})
	if err != nil {
		t.Fatalf("PresignParts: %v", err)
	}
	putPart(t, nu[0].URL, []byte("x"))
	if err := s.CompleteUpload(ctx, nh, MustParsePath("/keep.txt")); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if err := s.Restore(ctx, againID, MustParsePath("/keep.txt")); !errors.Is(err, ErrExist) {
		t.Fatalf("Restore over a file = %v, want ErrExist", err)
	}

	// Purging removes for good and is idempotent.
	if err := s.PurgeTrash(ctx, againID); err != nil {
		t.Fatalf("PurgeTrash: %v", err)
	}
	if err := s.PurgeTrash(ctx, againID); err != nil {
		t.Fatalf("second PurgeTrash: %v", err)
	}
	if _, err := s.TrashStat(ctx, againID); !errors.Is(err, ErrNotExist) {
		t.Fatalf("TrashStat after purge = %v, want ErrNotExist", err)
	}
	if err := s.Restore(ctx, againID, MustParsePath("/keep.txt")); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Restore after purge = %v, want ErrNotExist", err)
	}
}

func TestS3TrashRefusals(t *testing.T) {
	s := newTestS3(t, newStubS3("f.txt"), nil)
	ctx := context.Background()

	if _, err := s.Trash(ctx, MustParsePath("/")); err == nil {
		t.Error("Trash of the root succeeded")
	}
	if _, err := s.Trash(ctx, MustParsePath("/missing")); !errors.Is(err, ErrNotExist) {
		t.Errorf("Trash of a missing path = %v, want ErrNotExist", err)
	}
	if err := s.Restore(ctx, "bogus", MustParsePath("/x")); !errors.Is(err, ErrNotExist) {
		t.Errorf("Restore of a bogus id = %v, want ErrNotExist", err)
	}

	readOnly := newTestS3(t, newStubS3("f.txt"), func(cfg *S3Config) { cfg.ReadOnly = true })
	if _, err := readOnly.Trash(ctx, MustParsePath("/f.txt")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Trash in read-only mode = %v, want ErrReadOnly", err)
	}
}
