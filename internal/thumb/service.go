package thumb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/krishna2206/zefile/internal/job"
	"github.com/krishna2206/zefile/internal/storage"
)

// Thumb dimensions: fit within a 256px square, JPEG-encoded.
const (
	maxSide = 256
	quality = 70
)

// ffmpegTimeout bounds one video frame extraction.
const ffmpegTimeout = 3 * time.Minute

// failCooldown is how long a failed source waits before another attempt,
// so a missing ffmpeg binary or a corrupt file does not enqueue on every view.
const failCooldown = time.Hour

// ErrNoFFmpeg means the ffmpeg binary is not on PATH. Video sources stay
// without thumbnails until it appears.
var ErrNoFFmpeg = fmt.Errorf("thumb: ffmpeg is not installed")

type Kind int

const (
	KindImage Kind = iota
	KindVideo
	KindPDF
)

var imageExts = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "gif": true, "webp": true,
}

var videoExts = map[string]bool{
	"mp4": true, "webm": true, "mov": true, "m4v": true, "ogv": true,
	"mkv": true, "avi": true,
}

var pdfExts = map[string]bool{
	"pdf": true,
}

// Eligible reports whether name and size qualify for a generated thumbnail.
func Eligible(name string, size, maxBytes int64) (Kind, bool) {
	if size <= 0 || size > maxBytes {
		return 0, false
	}
	dot := strings.LastIndex(name, ".")
	if dot < 0 {
		return 0, false
	}
	ext := strings.ToLower(name[dot+1:])
	switch {
	case imageExts[ext]:
		return KindImage, true
	case videoExts[ext]:
		return KindVideo, true
	case pdfExts[ext]:
		return KindPDF, true
	}
	return 0, false
}

// Key identifies a thumbnail: the format version plus the source path,
// size and modification time, so a replaced file — or a newer thumbnail
// format — regenerates instead of serving a stale frame.
func Key(path string, size int64, modTime time.Time) string {
	sum := sha256.New()
	fmt.Fprintf(sum, "thumb2\x00%s\x00%d\x00%d", path, size, modTime.UnixNano())
	return hex.EncodeToString(sum.Sum(nil))
}

// Enqueuer receives background work. *job.Service satisfies it.
type Enqueuer interface {
	Enqueue(ctx context.Context, t job.Type, payload any) (job.Job, error)
}

// Service serves stored thumbnails and grows them in the background.
type Service struct {
	fs       storage.FS
	store    storage.ThumbStore
	jobs     Enqueuer
	maxBytes int64
	runVideo func(ctx context.Context, src, dst string) error
	runPDF   func(ctx context.Context, src, dst string) error

	mu       sync.Mutex
	inflight map[string]bool
	failed   map[string]time.Time
}

// New builds a Service. runVideo extracts one frame with ffmpeg; nil uses
// the ffmpeg binary on PATH.
func New(fs storage.FS, store storage.ThumbStore, jobs Enqueuer, maxBytes int64, runVideo func(context.Context, string, string) error) *Service {
	if runVideo == nil {
		runVideo = runFFmpeg
	}
	return &Service{
		fs: fs, store: store, jobs: jobs, maxBytes: maxBytes,
		runVideo: runVideo, runPDF: runPDFium,
		inflight: make(map[string]bool), failed: make(map[string]time.Time),
	}
}

// Get returns the stored thumbnail for p, or false when none exists yet.
// On a miss it asks the worker to grow one, guarded so repeated views do
// not pile up duplicate jobs.
func (s *Service) Get(ctx context.Context, p storage.Path, userID int64, isAdmin bool) ([]byte, bool, error) {
	info, err := s.fs.Stat(ctx, p)
	if err != nil {
		return nil, false, err
	}
	if info.IsDir {
		return nil, false, storage.ErrIsDir
	}
	if _, ok := Eligible(p.Name(), info.Size, s.maxBytes); !ok {
		return nil, false, nil
	}
	key := Key(p.String(), info.Size, info.ModTime)
	thumb, err := s.store.ReadThumb(ctx, key)
	if err == nil {
		return thumb, true, nil
	}
	if !isNotExist(err) {
		return nil, false, err
	}
	s.maybeEnqueue(ctx, key, job.ThumbPayload{
		Path:    p.String(),
		Size:    info.Size,
		ModTime: info.ModTime.UnixNano(),
		Key:     key,
		UserID:  userID,
		IsAdmin: isAdmin,
	})
	return nil, false, nil
}

// Request enqueues thumbnail generation for a freshly stored file.
// Errors are swallowed: thumbnails are best-effort decoration.
func (s *Service) Request(ctx context.Context, p storage.Path, userID int64, isAdmin bool) {
	info, err := s.fs.Stat(ctx, p)
	if err != nil || info.IsDir {
		return
	}
	if _, ok := Eligible(p.Name(), info.Size, s.maxBytes); !ok {
		return
	}
	key := Key(p.String(), info.Size, info.ModTime)
	if _, err := s.store.ReadThumb(ctx, key); err == nil {
		return
	}
	s.maybeEnqueue(ctx, key, job.ThumbPayload{
		Path:    p.String(),
		Size:    info.Size,
		ModTime: info.ModTime.UnixNano(),
		Key:     key,
		UserID:  userID,
		IsAdmin: isAdmin,
	})
}

func (s *Service) maybeEnqueue(ctx context.Context, key string, payload job.ThumbPayload) {
	s.mu.Lock()
	if s.inflight[key] {
		s.mu.Unlock()
		return
	}
	if since, ok := s.failed[key]; ok && time.Since(since) < failCooldown {
		s.mu.Unlock()
		return
	}
	s.inflight[key] = true
	s.mu.Unlock()

	if _, err := s.jobs.Enqueue(ctx, job.TypeThumb, payload); err != nil {
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
	}
}

// Generate grows the thumbnail a job payload names. It is safe to retry:
// the source is re-checked and the stored object is overwritten whole.
func (s *Service) Generate(ctx context.Context, payload job.ThumbPayload) error {
	defer func() {
		s.mu.Lock()
		delete(s.inflight, payload.Key)
		s.mu.Unlock()
	}()

	p, err := storage.ParsePath(payload.Path)
	if err != nil {
		return err
	}
	info, err := s.fs.Stat(ctx, p)
	if err != nil {
		return err // gone or unreadable: nothing to depict
	}
	if info.IsDir || info.Size != payload.Size || info.ModTime.UnixNano() != payload.ModTime {
		return nil // changed under the job; a newer request owns it
	}
	kind, ok := Eligible(p.Name(), info.Size, s.maxBytes)
	if !ok {
		return nil
	}
	if Key(p.String(), info.Size, info.ModTime) != payload.Key {
		return nil
	}

	f, err := s.fs.Open(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	tmp, err := os.CreateTemp("", "zefile-thumb-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, f); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	out := tmpName + ".jpg"
	defer func() { _ = os.Remove(out) }()
	var render error
	switch kind {
	case KindVideo:
		render = s.runVideo(ctx, tmpName, out)
	case KindPDF:
		render = s.runPDF(ctx, tmpName, out)
	default:
		render = makeImageThumb(tmpName, out)
	}
	if render != nil {
		s.noteFailed(payload.Key)
		return render
	}
	thumb, err := os.ReadFile(out)
	if err != nil {
		return err
	}
	if err := s.store.WriteThumb(ctx, payload.Key, thumb); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.failed, payload.Key)
	s.mu.Unlock()
	return nil
}

func (s *Service) noteFailed(key string) {
	s.mu.Lock()
	s.failed[key] = time.Now()
	s.mu.Unlock()
}

func isNotExist(err error) bool {
	return errors.Is(err, storage.ErrNotExist)
}

// runFFmpeg extracts one frame near the start, scaled to fit maxSide.
func runFFmpeg(ctx context.Context, src, dst string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return ErrNoFFmpeg
	}
	ctx, cancel := context.WithTimeout(ctx, ffmpegTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y", "-v", "error",
		"-ss", "1", "-i", src,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", maxSide, maxSide),
		"-q:v", "4",
		dst,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("thumb: ffmpeg: %v: %s", err, out)
	}
	return nil
}

// makeImageThumb decodes src, fits it in maxSide and writes a JPEG.
func makeImageThumb(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	img, _, err := image.Decode(in)
	if err != nil {
		return fmt.Errorf("thumb: decode: %w", err)
	}
	fit := fitWithin(img, maxSide)
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if err := jpeg.Encode(out, fit, &jpeg.Options{Quality: quality}); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// fitWithin scales src down to fit within a side square, keeping aspect
// ratio. Smaller images pass through untouched.
func fitWithin(src image.Image, side int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= side && h <= side {
		return src
	}
	var nw, nh int
	if w >= h {
		nw, nh = side, h*side/w
	} else {
		nw, nh = w*side/h, side
	}
	dst := image.NewRGBA(image.Rect(0, 0, max(nw, 1), max(nh, 1)))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}
