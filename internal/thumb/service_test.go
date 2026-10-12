package thumb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/krishna2206/zefile/internal/job"
	"github.com/krishna2206/zefile/internal/storage"
)

func TestEligible(t *testing.T) {
	const cap = 512 << 20
	for _, tc := range []struct {
		name     string
		size     int64
		wantKind Kind
		wantElig bool
	}{
		{"photo.jpg", 100, KindImage, true},
		{"clip.MP4", 100, KindVideo, true},
		{"paper.pdf", 100, KindPDF, true},
		{"paper.PDF", 100, KindPDF, true},
		{"notes.txt", 100, 0, false},
		{"archive.zip", 100, 0, false},
		{"photo.jpg", 0, 0, false},
		{"clip.mp4", cap + 1, 0, false},
		{"noext", 100, 0, false},
	} {
		kind, ok := Eligible(tc.name, tc.size, cap)
		if ok != tc.wantElig || (ok && kind != tc.wantKind) {
			t.Errorf("Eligible(%s, %d) = %v, %v; want %v, %v",
				tc.name, tc.size, kind, ok, tc.wantKind, tc.wantElig)
		}
	}
}

func TestKeyChangesWithVersion(t *testing.T) {
	a := Key("/a.png", 10, time.Unix(1, 0))
	if a != Key("/a.png", 10, time.Unix(1, 0)) {
		t.Error("Key is not deterministic")
	}
	if a == Key("/a.png", 11, time.Unix(1, 0)) || a == Key("/b.png", 10, time.Unix(1, 0)) {
		t.Error("Key ignores size or path")
	}
	if len(a) != 64 {
		t.Errorf("Key = %q, want 64 hex chars", a)
	}
}

type recordingJobs struct {
	mu       sync.Mutex
	payloads []job.ThumbPayload
}

func (r *recordingJobs) Enqueue(_ context.Context, _ job.Type, payload any) (job.Job, error) {
	r.mu.Lock()
	r.payloads = append(r.payloads, payload.(job.ThumbPayload))
	r.mu.Unlock()
	return job.Job{}, nil
}

func (r *recordingJobs) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.payloads)
}

func testFS(t *testing.T) *storage.Local {
	t.Helper()
	fs, err := storage.Open(storage.Config{Root: t.TempDir(), Reserve: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs
}

func seedPNG(t *testing.T, fs *storage.Local, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	f, err := fs.Create(context.Background(), storage.MustParsePath(path))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestImageRoundTrip(t *testing.T) {
	ctx := context.Background()
	fs := testFS(t)
	jobs := &recordingJobs{}
	svc := New(fs, fs, jobs, 512<<20, nil)
	p := storage.MustParsePath("/photo.png")
	seedPNG(t, fs, "/photo.png", 300, 200)

	if _, hit, err := svc.Get(ctx, p, 1, false); err != nil || hit {
		t.Fatalf("Get = hit %v, err %v; want a miss", hit, err)
	}
	if n := jobs.count(); n != 1 {
		t.Fatalf("enqueued %d jobs, want 1", n)
	}
	// A second view while the job is in flight enqueues nothing more.
	if _, _, err := svc.Get(ctx, p, 1, false); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if n := jobs.count(); n != 1 {
		t.Fatalf("enqueued %d jobs, want still 1", n)
	}

	payload := jobs.payloads[0]
	if err := svc.Generate(ctx, payload); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	thumb, hit, err := svc.Get(ctx, p, 1, false)
	if err != nil || !hit {
		t.Fatalf("Get after generate = hit %v, err %v", hit, err)
	}
	img, err := jpeg.Decode(bytes.NewReader(thumb))
	if err != nil {
		t.Fatalf("stored bytes are not a JPEG: %v", err)
	}
	b := img.Bounds()
	if b.Dx() > maxSide || b.Dy() > maxSide {
		t.Fatalf("thumbnail is %dx%d, want within %d", b.Dx(), b.Dy(), maxSide)
	}
	// A 300x200 source must fill the box: 256 on the long side.
	if b.Dx() != 256 || b.Dy() != 170 {
		t.Fatalf("thumbnail is %dx%d, want 256x170", b.Dx(), b.Dy())
	}
}

func TestGenerateSkipsChangedFile(t *testing.T) {
	ctx := context.Background()
	fs := testFS(t)
	jobs := &recordingJobs{}
	svc := New(fs, fs, jobs, 512<<20, nil)
	seedPNG(t, fs, "/photo.png", 60, 40)
	p := storage.MustParsePath("/photo.png")

	if _, _, err := svc.Get(ctx, p, 1, false); err != nil {
		t.Fatalf("Get: %v", err)
	}
	payload := jobs.payloads[0]

	// Replace the file: same path, new bytes and time.
	if err := fs.Remove(ctx, p); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	seedPNG(t, fs, "/photo.png", 61, 41)

	if err := svc.Generate(ctx, payload); err != nil {
		t.Fatalf("Generate on a changed file = %v, want nil (skip)", err)
	}
	if _, hit, _ := svc.Get(ctx, p, 1, false); hit {
		t.Fatal("changed file has a thumbnail, want a miss")
	}
}

func TestFailedSourceCoolsDown(t *testing.T) {
	ctx := context.Background()
	fs := testFS(t)
	jobs := &recordingJobs{}
	broken := func(context.Context, string, string) error {
		return errFakeFFmpeg
	}
	svc := New(fs, fs, jobs, 512<<20, broken)

	w, err := fs.Create(ctx, storage.MustParsePath("/clip.mp4"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte("not really a video")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p := storage.MustParsePath("/clip.mp4")

	if _, _, err := svc.Get(ctx, p, 1, false); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := svc.Generate(ctx, jobs.payloads[0]); err == nil {
		t.Fatal("Generate with a broken runner succeeded, want an error")
	}
	// The failure cools down: further views do not pile up jobs.
	if _, _, err := svc.Get(ctx, p, 1, false); err != nil {
		t.Fatalf("Get after failure: %v", err)
	}
	if n := jobs.count(); n != 1 {
		t.Fatalf("enqueued %d jobs, want still 1", n)
	}
}

var errFakeFFmpeg = errors.New("ffmpeg exploded")

// basic pdf for testing
func onePagePDF() []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 100] /Contents 4 0 R >>",
		"<< /Length 31 >>\nstream\n1 0 0 rg 20 20 160 60 re f\nendstream",
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, body := range objects {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}

	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objects)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, xref)
	return buf.Bytes()
}

func seedFile(t *testing.T, fs *storage.Local, path string, data []byte) {
	t.Helper()
	f, err := fs.Create(context.Background(), storage.MustParsePath(path))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestPDFFirstPageIsRendered(t *testing.T) {
	ctx := context.Background()
	fs := testFS(t)
	jobs := &recordingJobs{}
	svc := New(fs, fs, jobs, 512<<20, nil)
	seedFile(t, fs, "/paper.pdf", onePagePDF())
	p := storage.MustParsePath("/paper.pdf")

	if _, hit, err := svc.Get(ctx, p, 1, false); err != nil || hit {
		t.Fatalf("Get = hit %v, err %v; want a miss", hit, err)
	}
	if n := jobs.count(); n != 1 {
		t.Fatalf("enqueued %d jobs, want 1", n)
	}
	if err := svc.Generate(ctx, jobs.payloads[0]); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	thumb, hit, err := svc.Get(ctx, p, 1, false)
	if err != nil || !hit {
		t.Fatalf("Get after generate = hit %v, err %v", hit, err)
	}
	img, err := jpeg.Decode(bytes.NewReader(thumb))
	if err != nil {
		t.Fatalf("stored bytes are not a JPEG: %v", err)
	}
	// A 200x100 page fits the 256 box on its long side.
	if b := img.Bounds(); b.Dx() != maxSide || b.Dy() != maxSide/2 {
		t.Fatalf("thumbnail is %dx%d, want %dx%d", b.Dx(), b.Dy(), maxSide, maxSide/2)
	}
}

func TestBrokenPDFFails(t *testing.T) {
	ctx := context.Background()
	fs := testFS(t)
	jobs := &recordingJobs{}
	svc := New(fs, fs, jobs, 512<<20, nil)
	seedFile(t, fs, "/broken.pdf", []byte("%PDF-1.4\nnot really a pdf"))

	if _, _, err := svc.Get(ctx, storage.MustParsePath("/broken.pdf"), 1, false); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := svc.Generate(ctx, jobs.payloads[0]); err == nil {
		t.Fatal("Generate on a corrupt PDF succeeded, want an error")
	}
}

func TestRealFFmpegGrowsVideoThumb(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not on PATH")
	}
	ctx := context.Background()
	fs := testFS(t)
	jobs := &recordingJobs{}
	svc := New(fs, fs, jobs, 512<<20, nil)

	// A two-second testcard, generated locally so the test needs no fixtures.
	src := filepath.Join(t.TempDir(), "src.mp4")
	gen := exec.CommandContext(ctx, "ffmpeg", "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=10",
		"-pix_fmt", "yuv420p", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("make test video: %v: %s", err, out)
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read test video: %v", err)
	}
	w, err := fs.Create(ctx, storage.MustParsePath("/clip.mp4"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p := storage.MustParsePath("/clip.mp4")

	if _, _, err := svc.Get(ctx, p, 1, false); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := svc.Generate(ctx, jobs.payloads[0]); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	thumb, hit, err := svc.Get(ctx, p, 1, false)
	if err != nil || !hit {
		t.Fatalf("Get after generate = hit %v, err %v", hit, err)
	}
	img, err := jpeg.Decode(bytes.NewReader(thumb))
	if err != nil {
		t.Fatalf("stored bytes are not a JPEG: %v", err)
	}
	if b := img.Bounds(); b.Dx() > maxSide || b.Dy() > maxSide {
		t.Fatalf("thumbnail is %dx%d, want within %d", b.Dx(), b.Dy(), maxSide)
	}
}
