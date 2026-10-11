package storage

import (
	"context"
	"errors"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

var (
	_ Walker = (*Local)(nil)
	_ Walker = (*S3)(nil)
)

// Walker lists live files with sizes for usage accounting.
type Walker interface {
	Walk(ctx context.Context, fn func(path string, size int64) error) error
}

func firstComponent(rel string) string {
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return rel
}

// Walk lists live files under the root.
func (l *Local) Walk(ctx context.Context, fn func(path string, size int64) error) error {
	return l.walkDir(ctx, ".", "", fn)
}

func (l *Local) walkDir(ctx context.Context, dir, prefix string, fn func(path string, size int64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := l.root.Open(dir)
	if err != nil {
		return mapErr(err)
	}
	entries, err := f.ReadDir(-1)
	cerr := f.Close()
	if err != nil {
		return mapErr(err)
	}
	if cerr != nil {
		return mapErr(cerr)
	}
	for _, e := range entries {
		name := e.Name()
		rel := name
		if dir != "." {
			rel = path.Join(dir, name)
		}
		key := "/" + rel
		if prefix == "" && isReservedName(name) {
			continue
		}
		if e.IsDir() {
			if err := l.walkDir(ctx, rel, key, fn); err != nil {
				return err
			}
			continue
		}
		info, err := l.root.Lstat(rel)
		if err != nil {
			if mapped := mapErr(err); errors.Is(mapped, ErrNotExist) {
				continue
			}
			return mapErr(err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := fn(key, info.Size()); err != nil {
			return err
		}
	}
	return nil
}

// Walk lists live objects under the tree prefix.
func (s *S3) Walk(ctx context.Context, fn func(path string, size int64) error) error {
	base := ""
	if s.prefix != "" {
		base = s.prefix + "/"
	}
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(base),
			ContinuationToken: token,
		})
		if err != nil {
			return mapS3Err(err)
		}
		for _, obj := range out.Contents {
			key := aws.ToString(obj.Key)
			rel := strings.TrimPrefix(key, base)
			if rel == "" || strings.HasSuffix(rel, "/") {
				continue
			}
			if isReservedName(firstComponent(rel)) {
				continue
			}
			if err := fn("/"+rel, aws.ToInt64(obj.Size)); err != nil {
				return err
			}
		}
		if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			break
		}
		token = out.NextContinuationToken
	}
	return nil
}
