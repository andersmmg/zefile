package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// MaxThumbBytes caps a stored thumbnail. Generated ones are kilobytes; the
// cap only bites on foreign objects planted under the prefix by hand.
const MaxThumbBytes = 1 << 20

// ThumbStore keeps backend-generated thumbnails: small JPEGs keyed by an
// opaque content hash, hidden under a reserved prefix in both backends.
//
// Reads and writes bypass the Guard like trash internals do: the caller
// authorized the source file, and these keys are Zefile's own working data,
// never user paths.
type ThumbStore interface {
	ReadThumb(ctx context.Context, key string) ([]byte, error)
	WriteThumb(ctx context.Context, key string, thumb []byte) error
}

var (
	_ ThumbStore = (*Local)(nil)
	_ ThumbStore = (*S3)(nil)
)

// validThumbKey accepts only hex content hashes, so a key can never escape
// the thumbs directory.
func validThumbKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (l *Local) thumbPath(key string) string {
	return path.Join(ThumbsDir, key+".jpg")
}

func (l *Local) ReadThumb(_ context.Context, key string) ([]byte, error) {
	if !validThumbKey(key) {
		return nil, fmt.Errorf("%w: bad thumbnail key", ErrInvalidPath)
	}
	f, err := l.root.Open(l.thumbPath(key))
	if err != nil {
		return nil, mapErr(err)
	}
	defer func() { _ = f.Close() }()
	return readCapped(f)
}

func (l *Local) WriteThumb(_ context.Context, key string, thumb []byte) error {
	if !validThumbKey(key) {
		return fmt.Errorf("%w: bad thumbnail key", ErrInvalidPath)
	}
	if len(thumb) > MaxThumbBytes {
		return fmt.Errorf("%w: thumbnail is %d bytes", ErrTooLarge, len(thumb))
	}
	if err := l.root.Mkdir(ThumbsDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return mapErr(err)
	}
	f, err := l.root.OpenFile(l.thumbPath(key), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return mapErr(err)
	}
	if _, err := f.Write(thumb); err != nil {
		_ = f.Close()
		return mapErr(err)
	}
	return mapErr(f.Close())
}

func (s *S3) thumbKey(key string) string {
	if s.prefix == "" {
		return ThumbsDir + "/" + key + ".jpg"
	}
	return s.prefix + "/" + ThumbsDir + "/" + key + ".jpg"
}

func (s *S3) ReadThumb(ctx context.Context, key string) ([]byte, error) {
	if !validThumbKey(key) {
		return nil, fmt.Errorf("%w: bad thumbnail key", ErrInvalidPath)
	}
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.thumbKey(key)),
	})
	if err != nil {
		return nil, mapS3Err(err)
	}
	defer func() { _ = out.Body.Close() }()
	return readCapped(out.Body)
}

func (s *S3) WriteThumb(ctx context.Context, key string, thumb []byte) error {
	if !validThumbKey(key) {
		return fmt.Errorf("%w: bad thumbnail key", ErrInvalidPath)
	}
	if len(thumb) > MaxThumbBytes {
		return fmt.Errorf("%w: thumbnail is %d bytes", ErrTooLarge, len(thumb))
	}
	_, err := s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.thumbKey(key)),
		Body:        bytes.NewReader(thumb),
		ContentType: aws.String("image/jpeg"),
	})
	return mapS3Err(err)
}

// readCapped reads a stored thumbnail, refusing anything past the cap.
func readCapped(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxThumbBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxThumbBytes {
		return nil, fmt.Errorf("%w: stored thumbnail exceeds %d bytes", ErrTooLarge, MaxThumbBytes)
	}
	return b, nil
}
