package storage

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// Server-side reads for the S3 backend: an explicit exemption to the
// no-bytes-through-the-server rule, so previews, bundles and text
// extraction work in S3 mode. Uploads and downloads still bypass the
// server; only these bounded reads proxy through it.

// Open implements [FS] as ranged GetObject calls behind a seekable reader.
// Sequential reads stream one connection; each Seek opens a new range.
func (s *S3) Open(ctx context.Context, p Path) (File, error) {
	if err := authorize(ctx, s.guard, OpRead, p); err != nil {
		return nil, err
	}
	if p.IsRoot() {
		return nil, ErrIsDir
	}
	out, err := s.head(ctx, s.key(p))
	if err != nil {
		return nil, err
	}
	if out == nil {
		if exists, _, err := s.probeDir(ctx, s.dirPrefix(p)); err != nil {
			return nil, err
		} else if exists {
			return nil, ErrIsDir
		}
		return nil, ErrNotExist
	}
	return &s3File{
		ctx:     ctx,
		client:  s.client,
		bucket:  s.bucket,
		key:     s.key(p),
		name:    p.Name(),
		size:    aws.ToInt64(out.ContentLength),
		modTime: timeOrZero(out.LastModified),
	}, nil
}

// s3File is one open object: range reads issued as the consumer seeks.
type s3File struct {
	ctx     context.Context
	client  *awss3.Client
	bucket  string
	key     string
	name    string
	size    int64
	modTime time.Time
	off     int64
	body    io.ReadCloser
}

func (f *s3File) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if f.off >= f.size {
		return 0, io.EOF
	}
	if f.body == nil {
		if err := f.openStream(); err != nil {
			return 0, err
		}
	}
	n, err := f.body.Read(p)
	f.off += int64(n)
	if err != nil {
		f.closeBody()
		if err == io.EOF && n > 0 {
			err = nil
		}
	}
	return n, err
}

func (f *s3File) openStream() error {
	in := &awss3.GetObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(f.key)}
	if f.off > 0 {
		in.Range = aws.String(fmt.Sprintf("bytes=%d-", f.off))
	}
	out, err := f.client.GetObject(f.ctx, in)
	if err != nil {
		return mapS3Err(err)
	}
	f.body = out.Body
	return nil
}

func (f *s3File) Seek(offset int64, whence int) (int64, error) {
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = f.off
	case io.SeekEnd:
		base = f.size
	default:
		return 0, fmt.Errorf("storage: invalid seek origin %d", whence)
	}
	off := base + offset
	if off < 0 {
		return 0, fmt.Errorf("storage: invalid seek offset %d", off)
	}
	if off != f.off {
		f.closeBody()
		f.off = off
	}
	return off, nil
}

func (f *s3File) Close() error {
	f.closeBody()
	return nil
}

func (f *s3File) closeBody() {
	if f.body != nil {
		_ = f.body.Close()
		f.body = nil
	}
}

func (f *s3File) Stat() (fs.FileInfo, error) {
	return trashFile{name: f.name, size: f.size, modTime: f.modTime}, nil
}
