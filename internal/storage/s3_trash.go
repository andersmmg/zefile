package storage

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Reversible deletion: a server-side copy into the reserved trash prefix
// followed by a delete of the originals. Directories move object by object.

var _ Trasher = (*S3)(nil)

// trashPrefix is where trashed entries park: <prefix>/.zefile-trash/<id>/…
func (s *S3) trashPrefix() string {
	if s.prefix == "" {
		return TrashDir + "/"
	}
	return s.prefix + "/" + TrashDir + "/"
}

// cleanTrashID strips separators before an identifier reaches a key join.
func cleanTrashID(id TrashID) string {
	return strings.NewReplacer("/", "", "\\", "", ".", "").Replace(string(id))
}

// Trash implements [Trasher].
func (s *S3) Trash(ctx context.Context, p Path) (TrashID, error) {
	if p.IsRoot() {
		return "", fmt.Errorf("%w: cannot remove the root", ErrInvalidPath)
	}
	if err := authorize(ctx, s.guard, OpDelete, p); err != nil {
		return "", err
	}
	if err := s.checkWritable(); err != nil {
		return "", err
	}

	info, err := s.Stat(ctx, p)
	if err != nil {
		return "", err
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("storage: name trashed entry: %w", err)
	}
	id := TrashID(base64.RawURLEncoding.EncodeToString(raw))
	dest := s.trashPrefix() + cleanTrashID(id) + "/"

	if info.IsDir {
		keys, err := s.listAllKeys(ctx, s.dirPrefix(p))
		if err != nil {
			return "", err
		}
		for _, k := range keys {
			rel := strings.TrimPrefix(k, s.dirPrefix(p))
			if err := s.copyKey(ctx, k, dest+rel); err != nil {
				return "", err
			}
		}
		if err := s.deleteKeys(ctx, keys); err != nil {
			return "", err
		}
		s.metaForget(ctx, p)
		s.metaInvalidateDir(ctx, p.Parent())
		return id, nil
	}

	if err := s.copyKey(ctx, s.key(p), dest+info.Name); err != nil {
		return "", err
	}
	_, err = s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(p)),
	})
	if err := mapS3Err(err); err != nil {
		return "", err
	}
	s.metaForget(ctx, p)
	s.metaInvalidateDir(ctx, p.Parent())
	return id, nil
}

// Restore implements [Trasher]. Same contract as Local.
func (s *S3) Restore(ctx context.Context, id TrashID, to Path) error {
	if to.IsRoot() {
		return fmt.Errorf("%w: cannot restore over the root", ErrInvalidPath)
	}
	if err := authorize(ctx, s.guard, OpWrite, to); err != nil {
		return err
	}
	if err := s.checkWritable(); err != nil {
		return err
	}

	src := s.trashPrefix() + cleanTrashID(id) + "/"
	keys, err := s.listAllKeys(ctx, src)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return ErrNotExist
	}
	if err := s.ensureDir(ctx, to.Parent()); err != nil {
		return err
	}
	taken, err := s.exists(ctx, to)
	if err != nil {
		return err
	}
	if taken {
		return ErrExist
	}

	if len(keys) == 1 && !strings.Contains(strings.TrimPrefix(keys[0], src), "/") {
		if err := s.copyKey(ctx, keys[0], s.key(to)); err != nil {
			return err
		}
		if err := s.deleteKeys(ctx, keys); err != nil {
			return err
		}
		// The restored content is new to the tree: drop any stale row
		// at the destination and let the next read repopulate it.
		s.metaForget(ctx, to)
		s.metaInvalidateDir(ctx, to.Parent())
		return nil
	}

	for _, k := range keys {
		rel := strings.TrimPrefix(k, src)
		if err := s.copyKey(ctx, k, s.dirPrefix(to)+rel); err != nil {
			return err
		}
	}
	if err := s.deleteKeys(ctx, keys); err != nil {
		return err
	}
	s.metaForget(ctx, to)
	s.metaInvalidateDir(ctx, to.Parent())
	return nil
}

// PurgeTrash implements [Trasher]. It is idempotent.
func (s *S3) PurgeTrash(ctx context.Context, id TrashID) error {
	if err := s.checkWritable(); err != nil {
		return err
	}
	keys, err := s.listAllKeys(ctx, s.trashPrefix()+cleanTrashID(id)+"/")
	if err != nil {
		return err
	}
	return s.deleteKeys(ctx, keys)
}

// TrashStat implements [Trasher]. One top-level object is a file,
// anything else a directory.
func (s *S3) TrashStat(ctx context.Context, id TrashID) (os.FileInfo, error) {
	keys, err := s.listAllKeys(ctx, s.trashPrefix()+cleanTrashID(id)+"/")
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, ErrNotExist
	}
	src := s.trashPrefix() + cleanTrashID(id) + "/"
	if len(keys) == 1 && !strings.Contains(strings.TrimPrefix(keys[0], src), "/") {
		out, err := s.head(ctx, keys[0])
		if err != nil {
			return nil, err
		}
		if out == nil {
			return nil, ErrNotExist
		}
		return trashFile{
			name:    keys[0][strings.LastIndex(keys[0], "/")+1:],
			size:    aws.ToInt64(out.ContentLength),
			modTime: timeOrZero(out.LastModified),
		}, nil
	}
	return trashFile{name: string(id), isDir: true}, nil
}

type trashFile struct {
	name    string
	size    int64
	modTime time.Time
	isDir   bool
}

func (f trashFile) Name() string       { return f.name }
func (f trashFile) Size() int64        { return f.size }
func (f trashFile) Mode() os.FileMode  { return 0 }
func (f trashFile) ModTime() time.Time { return f.modTime }
func (f trashFile) IsDir() bool        { return f.isDir }
func (f trashFile) Sys() any           { return nil }

// listAllKeys returns every object key under a prefix, following pagination.
func (s *S3) listAllKeys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, mapS3Err(err)
		}
		for _, obj := range out.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}
		if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			break
		}
		token = out.NextContinuationToken
	}
	return keys, nil
}

// deleteKeys removes objects in batches of a thousand. An empty set
// succeeds.
func (s *S3) deleteKeys(ctx context.Context, keys []string) error {
	for len(keys) > 0 {
		n := min(len(keys), 1000)
		batch, rest := keys[:n], keys[n:]
		ids := make([]types.ObjectIdentifier, 0, len(batch))
		for _, k := range batch {
			ids = append(ids, types.ObjectIdentifier{Key: aws.String(k)})
		}
		_, err := s.client.DeleteObjects(ctx, &awss3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return mapS3Err(err)
		}
		keys = rest
	}
	return nil
}

// copyThreshold is where a server-side copy switches from one CopyObject
// to a multipart copy (CopyObject refuses objects past 5 GB).
const copyThreshold = 4 << 30

// copyPartSize is the range length multipart copies move per part.
const copyPartSize = 512 << 20

// copySource renders the x-amz-copy-source header value, escaping
// everything but slashes.
func (s *S3) copySource(from string) string {
	u := url.URL{Path: "/" + s.bucket + "/" + from}
	return strings.TrimPrefix(u.EscapedPath(), "/")
}

// copyKey copies one object server-side, scaling to a multipart copy
// past the single-request size limit. Metadata rides along.
func (s *S3) copyKey(ctx context.Context, from, to string) error {
	head, err := s.head(ctx, from)
	if err != nil {
		return err
	}
	if head == nil {
		return ErrNotExist
	}
	return s.copyKeySized(ctx, from, to, aws.ToInt64(head.ContentLength))
}

func (s *S3) copyKeySized(ctx context.Context, from, to string, size int64) error {
	if strings.HasSuffix(from, "/") {
		if size != 0 {
			return fmt.Errorf("storage: cannot copy %q: trailing-slash object holds data", from)
		}
		return s.putMarker(ctx, to)
	}
	if size < copyThreshold {
		_, err := s.client.CopyObject(ctx, &awss3.CopyObjectInput{
			Bucket:     aws.String(s.bucket),
			Key:        aws.String(to),
			CopySource: aws.String(s.copySource(from)),
		})
		return mapS3Err(err)
	}

	head, err := s.head(ctx, from)
	if err != nil {
		return err
	}
	if head == nil {
		return ErrNotExist
	}
	up, err := s.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket:             aws.String(s.bucket),
		Key:                aws.String(to),
		ContentType:        head.ContentType,
		ContentDisposition: head.ContentDisposition,
	})
	if err != nil {
		return mapS3Err(err)
	}
	uploadID := aws.ToString(up.UploadId)
	var completed []types.CompletedPart
	for start, n := int64(0), 1; start < size; n++ {
		end := min(start+copyPartSize, size) - 1
		out, err := s.client.UploadPartCopy(ctx, &awss3.UploadPartCopyInput{
			Bucket:     aws.String(s.bucket),
			Key:        aws.String(to),
			UploadId:   aws.String(uploadID),
			PartNumber: aws.Int32(int32(n)),
			CopySource: aws.String(s.copySource(from)),
			CopySourceRange: aws.String(
				fmt.Sprintf("bytes=%d-%d", start, end)),
		})
		if err != nil {
			_, _ = s.client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
				Bucket:   aws.String(s.bucket),
				Key:      aws.String(to),
				UploadId: aws.String(uploadID),
			})
			return mapS3Err(err)
		}
		completed = append(completed, types.CompletedPart{
			ETag:       out.CopyPartResult.ETag,
			PartNumber: aws.Int32(int32(n)),
		})
		start = end + 1
	}
	_, err = s.client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(to),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	return mapS3Err(err)
}
