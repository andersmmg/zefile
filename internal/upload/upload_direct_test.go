package upload_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krishna2206/zefile/internal/db"
	"github.com/krishna2206/zefile/internal/storage"
	"github.com/krishna2206/zefile/internal/upload"
)

// directFake is an in-memory DirectStager plus the Stat half of Store: it
// behaves like the S3 backend (opaque handle IDs, server-side part truth,
// refuse-if-exists) without touching the network.
type directFake struct {
	mu       sync.Mutex
	objects  map[string][]byte
	uploads  map[string]*fakeUpload
	next     int
	partSize int64
}

type fakeUpload struct {
	target storage.Path
	size   int64
	parts  map[int][]byte
}

func newDirectFake(partSize int64) *directFake {
	return &directFake{
		objects:  make(map[string][]byte),
		uploads:  make(map[string]*fakeUpload),
		partSize: partSize,
	}
}

func (f *directFake) Stat(_ context.Context, p storage.Path) (storage.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.objects[p.String()]
	if !ok {
		return storage.FileInfo{}, storage.ErrNotExist
	}
	return storage.FileInfo{Path: p, Name: p.Name(), Size: int64(len(body))}, nil
}

// The tus staging half is refused: in direct mode the service must never
// reach for it.
func (f *directFake) NewStage(context.Context) (storage.StageID, error) {
	return "", storage.ErrNotSupported
}

func (f *directFake) EnsureStage(context.Context, storage.StageID) (int64, error) {
	return 0, storage.ErrNotSupported
}

func (f *directFake) StageSize(context.Context, storage.StageID) (int64, error) {
	return 0, storage.ErrNotSupported
}

func (f *directFake) AppendToStage(context.Context, storage.StageID, int64, io.Reader) (int64, error) {
	return 0, storage.ErrNotSupported
}

func (f *directFake) CommitStage(context.Context, storage.StageID, storage.Path) error {
	return storage.ErrNotSupported
}

func (f *directFake) OpenStage(context.Context, storage.StageID) (storage.File, error) {
	return nil, storage.ErrNotSupported
}

func (f *directFake) DiscardStage(context.Context, storage.StageID) error {
	return storage.ErrNotSupported
}

func (f *directFake) PurgeStagesOlderThan(context.Context, time.Time) (int, error) {
	return 0, nil
}

func (f *directFake) BeginUpload(_ context.Context, target storage.Path, size int64) (storage.UploadHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, taken := f.objects[target.String()]; taken {
		return storage.UploadHandle{}, storage.ErrExist
	}
	f.next++
	id := fmt.Sprintf("up-%d", f.next)
	f.uploads[id] = &fakeUpload{target: target, size: size, parts: make(map[int][]byte)}
	return storage.UploadHandle{ID: id, PartSize: storage.PartSizeFor(f.partSize, size)}, nil
}

func (f *directFake) PresignParts(_ context.Context, h storage.UploadHandle, partNumbers []int) ([]storage.PartURL, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.uploads[h.ID]; !ok {
		return nil, fmt.Errorf("upload is gone: %w", storage.ErrNotExist)
	}
	if len(partNumbers) == 0 {
		return nil, errors.New("no part numbers requested")
	}
	urls := make([]storage.PartURL, 0, len(partNumbers))
	for _, n := range partNumbers {
		if n < 1 || n > storage.MaxMultipartParts {
			return nil, fmt.Errorf("part number %d out of range", n)
		}
		urls = append(urls, storage.PartURL{
			Number: n,
			URL:    fmt.Sprintf("fake://part/%s/%d", h.ID, n),
		})
	}
	return urls, nil
}

// putPart simulates the client PUTing bytes to a presigned URL.
func (f *directFake) putPart(url string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rest, ok := strings.CutPrefix(url, "fake://part/")
	if !ok {
		return fmt.Errorf("bad fake URL %q", url)
	}
	sep := strings.LastIndex(rest, "/")
	id, num := rest[:sep], rest[sep+1:]
	up, ok := f.uploads[id]
	if !ok {
		return fmt.Errorf("upload is gone: %w", storage.ErrNotExist)
	}
	var n int
	if _, err := fmt.Sscanf(num, "%d", &n); err != nil {
		return err
	}
	up.parts[n] = body
	return nil
}

func (f *directFake) CommittedBytes(_ context.Context, h storage.UploadHandle) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	up, ok := f.uploads[h.ID]
	if !ok {
		return 0, fmt.Errorf("upload is gone: %w", storage.ErrNotExist)
	}
	var total int64
	for _, b := range up.parts {
		total += int64(len(b))
	}
	return total, nil
}

func (f *directFake) CompleteUpload(_ context.Context, h storage.UploadHandle, to storage.Path) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	up, ok := f.uploads[h.ID]
	if !ok {
		return fmt.Errorf("upload is gone: %w", storage.ErrNotExist)
	}
	if up.target != to {
		return errors.New("handle is for another destination")
	}
	if _, taken := f.objects[to.String()]; taken {
		return storage.ErrExist
	}
	if len(up.parts) == 0 && up.size != 0 {
		return errors.New("nothing was uploaded")
	}
	var body []byte
	for n := 1; ; n++ {
		part, ok := up.parts[n]
		if !ok {
			break
		}
		body = append(body, part...)
	}
	f.objects[to.String()] = body
	delete(f.uploads, h.ID)
	return nil
}

func (f *directFake) AbortUpload(_ context.Context, h storage.UploadHandle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.uploads, h.ID)
	return nil
}

func newDirectHarness(t *testing.T, partSize int64, opts ...upload.Option) (*harness, *directFake) {
	t.Helper()

	database, err := db.Open(t.Context(), db.Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if _, err := database.Write.ExecContext(t.Context(),
		`INSERT INTO users (id, username, password_hash, created_at, updated_at)
		 VALUES (1, 'krishna', 'x', 0, 0)`); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	fake := newDirectFake(partSize)
	opts = append([]upload.Option{upload.WithDirect(fake, partSize)}, opts...)
	h := &harness{svc: upload.New(database, fake, opts...)}
	return h, fake
}

func TestDirectUploadFlow(t *testing.T) {
	t.Parallel()

	h, fake := newDirectHarness(t, 5<<20)
	ctx := t.Context()
	target := storage.MustParsePath("/film.mkv")
	payload := make([]byte, 12<<20)
	for i := range payload {
		payload[i] = byte(i * 13 % 251)
	}

	session, err := h.svc.Create(ctx, 1, target, int64(len(payload)), "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if session.Mode != upload.ModeDirect {
		t.Fatalf("Mode = %q, want %q", session.Mode, upload.ModeDirect)
	}
	if session.PartSize != 5<<20 {
		t.Fatalf("PartSize = %d, want %d", session.PartSize, 5<<20)
	}

	// Tus writes are refused for a direct session, at the service seam.
	if _, err := h.svc.Write(ctx, session.Token, 0, nil); !errors.Is(err, upload.ErrWrongMode) {
		t.Fatalf("Write on a direct session = %v, want ErrWrongMode", err)
	}

	// Three parts go up through the presigned URLs.
	urls, err := h.svc.PartURLs(ctx, session.Token, []int{1, 2, 3})
	if err != nil {
		t.Fatalf("PartURLs: %v", err)
	}
	for i, u := range urls {
		start := int64(i) * (5 << 20)
		end := min(start+(5<<20), int64(len(payload)))
		if err := fake.putPart(u.URL, payload[start:end]); err != nil {
			t.Fatalf("put part %d: %v", u.Number, err)
		}
	}

	offset, err := h.svc.Offset(ctx, session.Token)
	if err != nil {
		t.Fatalf("Offset: %v", err)
	}
	if offset.Offset != int64(len(payload)) {
		t.Fatalf("Offset = %d, want %d", offset.Offset, len(payload))
	}

	done, err := h.svc.Complete(ctx, session.Token)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !done.Complete {
		t.Fatal("Complete did not mark the session complete")
	}
	fake.mu.Lock()
	got := fake.objects[target.String()]
	fake.mu.Unlock()
	if string(got) != string(payload) {
		t.Fatal("completed object differs from what was sent")
	}

	// The session is gone: completing twice is not completing twice.
	if _, err := h.svc.Complete(ctx, session.Token); !errors.Is(err, upload.ErrNotFound) {
		t.Fatalf("second Complete = %v, want ErrNotFound", err)
	}
}

func TestDirectCompleteTooEarly(t *testing.T) {
	t.Parallel()

	h, fake := newDirectHarness(t, 5<<20)
	ctx := t.Context()

	session, err := h.svc.Create(ctx, 1, storage.MustParsePath("/partial.bin"), 10<<20, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	urls, err := h.svc.PartURLs(ctx, session.Token, []int{1})
	if err != nil {
		t.Fatalf("PartURLs: %v", err)
	}
	if err := fake.putPart(urls[0].URL, make([]byte, 5<<20)); err != nil {
		t.Fatalf("put part: %v", err)
	}
	if _, err := h.svc.Complete(ctx, session.Token); !errors.Is(err, upload.ErrSizeMismatch) {
		t.Fatalf("Complete at half bytes = %v, want ErrSizeMismatch", err)
	}
}

func TestDirectCancelAborts(t *testing.T) {
	t.Parallel()

	h, fake := newDirectHarness(t, 5<<20)
	ctx := t.Context()
	target := storage.MustParsePath("/cancelled.bin")

	session, err := h.svc.Create(ctx, 1, target, 10, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.svc.Cancel(ctx, session.Token); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// Cancelling twice is still not an error.
	if err := h.svc.Cancel(ctx, session.Token); err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
	// The backend upload is gone, and the destination is free again.
	fake.mu.Lock()
	uploadsLeft := len(fake.uploads)
	fake.mu.Unlock()
	if uploadsLeft != 0 {
		t.Fatalf("%d backend uploads left after cancel", uploadsLeft)
	}
	if _, err := h.svc.Create(ctx, 1, target, 10, ""); err != nil {
		t.Fatalf("Create after cancel: %v", err)
	}
}

func TestDirectCreateExistingTarget(t *testing.T) {
	t.Parallel()

	h, fake := newDirectHarness(t, 5<<20)
	ctx := t.Context()
	target := storage.MustParsePath("/taken.bin")
	fake.objects[target.String()] = []byte("someone was here")

	if _, err := h.svc.Create(ctx, 1, target, 10, ""); !errors.Is(err, storage.ErrExist) {
		t.Fatalf("Create over a file = %v, want ErrExist", err)
	}
}

func TestDirectPurgeExpiresAborts(t *testing.T) {
	t.Parallel()

	now := time.Now()
	h, fake := newDirectHarness(t, 5<<20, upload.WithClock(func() time.Time { return now }))
	ctx := t.Context()

	if _, err := h.svc.Create(ctx, 1, storage.MustParsePath("/stale.bin"), 10, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	now = now.Add(25 * time.Hour) // past the default TTL
	n, err := h.svc.PurgeExpired(ctx)
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("PurgeExpired removed %d sessions, want 1", n)
	}
	fake.mu.Lock()
	uploadsLeft := len(fake.uploads)
	fake.mu.Unlock()
	if uploadsLeft != 0 {
		t.Fatal("expired session's backend upload survived the purge")
	}
}

func TestTusServiceRejectsDirectCalls(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := t.Context()

	session, err := h.svc.Create(ctx, 1, storage.MustParsePath("/plain.bin"), 10, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if session.Mode != upload.ModeTus {
		t.Fatalf("Mode = %q, want %q", session.Mode, upload.ModeTus)
	}
	if _, err := h.svc.PartURLs(ctx, session.Token, []int{1}); !errors.Is(err, upload.ErrWrongMode) {
		t.Fatalf("PartURLs on a tus session = %v, want ErrWrongMode", err)
	}
	if _, err := h.svc.Complete(ctx, session.Token); !errors.Is(err, upload.ErrWrongMode) {
		t.Fatalf("Complete on a tus session = %v, want ErrWrongMode", err)
	}
}
