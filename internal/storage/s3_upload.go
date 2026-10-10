package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Direct upload and presigned download: bytes move between client and
// bucket; this process issues only capability URLs and metadata calls.

// handleSeparator joins the provider upload ID and the object key inside
// an [UploadHandle]'s ID, keeping every operation stateless. Upload IDs
// never contain NUL; a key that does is refused.
const handleSeparator = "\x00"

func encodeUploadHandle(uploadID, key string) string {
	return uploadID + handleSeparator + key
}

func decodeUploadHandle(id string) (uploadID, key string, err error) {
	uploadID, key, found := strings.Cut(id, handleSeparator)
	if !found || key == "" || strings.ContainsRune(key, '\x00') {
		return "", "", fmt.Errorf("storage: s3 upload: malformed handle")
	}
	return uploadID, key, nil
}

var _ DirectStager = (*S3)(nil)
var _ Presigner = (*S3)(nil)

// signer mints capability URLs against the public address when one is
// configured, the endpoint otherwise.
func (s *S3) signer() *awss3.PresignClient {
	if s.public != nil {
		return s.public
	}
	return s.presign
}

// BeginUpload implements [DirectStager]. An empty file needs no
// multipart upload — S3 refuses to complete one with zero parts — so its
// empty object is written now and completing only re-checks it.
func (s *S3) BeginUpload(ctx context.Context, target Path, size int64) (UploadHandle, error) {
	if err := authorize(ctx, s.guard, OpWrite, target); err != nil {
		return UploadHandle{}, err
	}
	if err := s.checkWritable(); err != nil {
		return UploadHandle{}, err
	}
	if size < 0 {
		return UploadHandle{}, fmt.Errorf("storage: s3 upload: negative size %d", size)
	}
	if err := s.ensureDir(ctx, target.Parent()); err != nil {
		return UploadHandle{}, err
	}
	taken, err := s.exists(ctx, target)
	if err != nil {
		return UploadHandle{}, err
	}
	if taken {
		return UploadHandle{}, ErrExist
	}

	key := s.key(target)
	partSize := PartSizeFor(s.partSize, size)
	if size == 0 {
		if err := s.putEmpty(ctx, key, target.Name()); err != nil {
			return UploadHandle{}, err
		}
		return UploadHandle{ID: encodeUploadHandle("", key), PartSize: partSize}, nil
	}

	out, err := s.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket:             aws.String(s.bucket),
		Key:                aws.String(key),
		ContentType:        aws.String(contentTypeFor(target.Name())),
		ContentDisposition: aws.String(attachmentDisposition(target.Name())),
	})
	if err != nil {
		return UploadHandle{}, mapS3Err(err)
	}
	return UploadHandle{ID: encodeUploadHandle(aws.ToString(out.UploadId), key), PartSize: partSize}, nil
}

// PresignParts implements [DirectStager]. Presigning is local
// cryptography; no request leaves this process.
func (s *S3) PresignParts(ctx context.Context, h UploadHandle, partNumbers []int) ([]PartURL, error) {
	uploadID, key, err := decodeUploadHandle(h.ID)
	if err != nil {
		return nil, err
	}
	if uploadID == "" {
		return nil, fmt.Errorf("storage: s3 upload: empty file has no parts")
	}
	if len(partNumbers) == 0 {
		return nil, fmt.Errorf("storage: s3 upload: no part numbers requested")
	}
	urls := make([]PartURL, 0, len(partNumbers))
	for _, n := range partNumbers {
		if n < 1 || n > MaxMultipartParts {
			return nil, fmt.Errorf("storage: s3 upload: part number %d out of range", n)
		}
		req, err := s.signer().PresignUploadPart(ctx, &awss3.UploadPartInput{
			Bucket:     aws.String(s.bucket),
			Key:        aws.String(key),
			UploadId:   aws.String(uploadID),
			PartNumber: aws.Int32(int32(n)),
		}, awss3.WithPresignExpires(s.presignTTL))
		if err != nil {
			return nil, fmt.Errorf("storage: s3 upload: presign part %d: %w", n, err)
		}
		urls = append(urls, PartURL{Number: n, URL: req.URL})
	}
	return urls, nil
}

// CommittedBytes implements [DirectStager] by summing the parts storage
// reports. A vanished upload answers [ErrNotExist].
func (s *S3) CommittedBytes(ctx context.Context, h UploadHandle) (int64, error) {
	uploadID, key, err := decodeUploadHandle(h.ID)
	if err != nil {
		return 0, err
	}
	if uploadID == "" {
		out, err := s.head(ctx, key)
		if err != nil {
			return 0, err
		}
		if out == nil {
			return 0, fmt.Errorf("storage: s3 upload: empty object is gone: %w", ErrNotExist)
		}
		return 0, nil
	}
	parts, err := s.listParts(ctx, key, uploadID)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, p := range parts {
		total += aws.ToInt64(p.Size)
	}
	return total, nil
}

// CompleteUpload implements [DirectStager]. The part list comes from
// storage (ListParts), not from the client, and the destination is
// re-checked still absent before the single Complete call.
func (s *S3) CompleteUpload(ctx context.Context, h UploadHandle, to Path) error {
	if err := authorize(ctx, s.guard, OpWrite, to); err != nil {
		return err
	}
	if err := s.checkWritable(); err != nil {
		return err
	}
	uploadID, key, err := decodeUploadHandle(h.ID)
	if err != nil {
		return err
	}
	if key != s.key(to) {
		return fmt.Errorf("storage: s3 upload: handle is for another destination")
	}
	if uploadID == "" {
		out, err := s.head(ctx, key)
		if err != nil {
			return err
		}
		if out == nil {
			return fmt.Errorf("storage: s3 upload: empty object is gone: %w", ErrNotExist)
		}
		return nil
	}

	parts, err := s.listParts(ctx, key, uploadID)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return fmt.Errorf("storage: s3 upload: nothing was uploaded")
	}
	if out, err := s.head(ctx, key); err != nil {
		return err
	} else if out != nil {
		return ErrExist
	}

	completed := make([]types.CompletedPart, 0, len(parts))
	for _, p := range parts {
		completed = append(completed, types.CompletedPart{
			ETag:       p.ETag,
			PartNumber: p.PartNumber,
		})
	}
	_, err = s.client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completed,
		},
	})
	return mapS3Err(err)
}

// AbortUpload implements [DirectStager]. It is idempotent. For an empty
// file the just-written object is deleted instead.
func (s *S3) AbortUpload(ctx context.Context, h UploadHandle) error {
	uploadID, key, err := decodeUploadHandle(h.ID)
	if err != nil {
		return err
	}
	if uploadID == "" {
		_, err := s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
		})
		return mapS3Err(err)
	}
	_, err = s.client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	if isNoSuchUpload(err) {
		return nil
	}
	return mapS3Err(err)
}

// PresignGet implements [Presigner]. The object is looked up first so a
// missing file fails under the stable error mapping. disposition is a
// per-request Content-Disposition override ("" keeps the stored one).
func (s *S3) PresignGet(ctx context.Context, p Path, head bool, ttl time.Duration, disposition string) (string, error) {
	if err := authorize(ctx, s.guard, OpRead, p); err != nil {
		return "", err
	}
	key := s.key(p)
	out, err := s.head(ctx, key)
	if err != nil {
		return "", err
	}
	if out == nil {
		return "", ErrNotExist
	}
	if ttl <= 0 {
		ttl = s.presignTTL
	}
	if head {
		in := &awss3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
		if disposition != "" {
			in.ResponseContentDisposition = aws.String(disposition)
		}
		req, err := s.signer().PresignHeadObject(ctx, in, awss3.WithPresignExpires(ttl))
		if err != nil {
			return "", fmt.Errorf("storage: s3 presign: %w", err)
		}
		return req.URL, nil
	}
	in := &awss3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if disposition != "" {
		in.ResponseContentDisposition = aws.String(disposition)
	}
	req, err := s.signer().PresignGetObject(ctx, in, awss3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("storage: s3 presign: %w", err)
	}
	return req.URL, nil
}

// listParts returns every part storage holds for an upload.
func (s *S3) listParts(ctx context.Context, key, uploadID string) ([]types.Part, error) {
	var parts []types.Part
	var marker *string
	for {
		out, err := s.client.ListParts(ctx, &awss3.ListPartsInput{
			Bucket:           aws.String(s.bucket),
			Key:              aws.String(key),
			UploadId:         aws.String(uploadID),
			PartNumberMarker: marker,
		})
		if err != nil {
			if isNoSuchUpload(err) {
				return nil, fmt.Errorf("storage: s3 upload: upload is gone: %w", ErrNotExist)
			}
			return nil, mapS3Err(err)
		}
		parts = append(parts, out.Parts...)
		if !aws.ToBool(out.IsTruncated) || out.NextPartNumberMarker == nil {
			break
		}
		marker = out.NextPartNumberMarker
	}
	return parts, nil
}

// putEmpty writes the zero-byte object an empty upload consists of.
func (s *S3) putEmpty(ctx context.Context, key, name string) error {
	_, err := s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:             aws.String(s.bucket),
		Key:                aws.String(key),
		Body:               bytes.NewReader(nil),
		ContentType:        aws.String(contentTypeFor(name)),
		ContentDisposition: aws.String(attachmentDisposition(name)),
	})
	return mapS3Err(err)
}

// isNoSuchUpload reports whether an upload ID is unknown to the provider.
func isNoSuchUpload(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchUpload" {
		return true
	}
	return false
}

// contentTypeFor names the object's type from its extension.
func contentTypeFor(name string) string {
	if ct := mime.TypeByExtension(path.Ext(strings.ToLower(name))); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// attachmentDisposition is the safe default every object carries: nothing
// renders in place unless a download overrides it per request.
func attachmentDisposition(name string) string {
	return "attachment; filename*=UTF-8''" + encodeRFC5987(name)
}

// encodeRFC5987 escapes a filename for a Content-Disposition header.
func encodeRFC5987(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.IndexByte("!#$&+-.^_`|~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
