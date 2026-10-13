package storage

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Local staging enables efficient, resumable, and idempotent streaming of
// files to S3.

var _ Stager = (*S3)(nil)

func (s *S3) stagePath(id StageID) (string, error) {
	raw := string(id)
	if raw == "" || strings.HasPrefix(raw, ".") || strings.ContainsAny(raw, `/\`) {
		return "", fmt.Errorf("%w: malformed stage id", ErrInvalidPath)
	}
	return filepath.Join(s.stageDir, raw), nil
}

// NewStage implements [Stager].
func (s *S3) NewStage(context.Context) (StageID, error) {
	if err := s.checkWritable(); err != nil {
		return "", err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("storage: name staged file: %w", err)
	}
	id := StageID(base64.RawURLEncoding.EncodeToString(raw))
	p, err := s.stagePath(id)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("storage: s3 stage: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("storage: s3 stage: %w", err)
	}
	return id, nil
}

// EnsureStage implements [Stager]: open the stage under a caller-chosen
// identifier, creating it empty when absent, and answer its size.
func (s *S3) EnsureStage(ctx context.Context, id StageID) (int64, error) {
	if err := s.checkWritable(); err != nil {
		return 0, err
	}
	if size, err := s.StageSize(ctx, id); err == nil {
		return size, nil
	} else if !errors.Is(err, ErrNoStagedFile) {
		return 0, err
	}
	p, err := s.stagePath(id)
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return 0, fmt.Errorf("storage: s3 stage: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("storage: s3 stage: %w", err)
	}
	return 0, nil
}

// StageSize implements [Stager].
func (s *S3) StageSize(_ context.Context, id StageID) (int64, error) {
	p, err := s.stagePath(id)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrNoStagedFile
		}
		return 0, fmt.Errorf("storage: s3 stage: %w", err)
	}
	return info.Size(), nil
}

// AppendToStage implements [Stager], allowing data to be written at a
// specific offset to support reliable transfer resumption
func (s *S3) AppendToStage(ctx context.Context, id StageID, offset int64, src io.Reader) (int64, error) {
	if err := s.checkWritable(); err != nil {
		return 0, err
	}
	if offset < 0 {
		return 0, fmt.Errorf("%w: negative offset", ErrInvalidPath)
	}
	current, err := s.StageSize(ctx, id)
	if err != nil {
		return 0, err
	}
	if offset > current {
		return 0, fmt.Errorf("%w: offset %d is beyond the %d bytes received", ErrInvalidPath, offset, current)
	}

	p, err := s.stagePath(id)
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrNoStagedFile
		}
		return 0, fmt.Errorf("storage: s3 stage: %w", err)
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("storage: s3 stage: %w", err)
	}
	written, err := io.Copy(f, ctxAwareReader{ctx: ctx, r: src})
	if err != nil {
		// allows for resuming transfer
		return offset + written, err
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("storage: s3 stage: %w", err)
	}
	return max(current, offset+written), nil
}

// CommitStage implements [Stager]: stream the finished file into the bucket.
// Authorisation runs here, when the bytes become visible.
func (s *S3) CommitStage(ctx context.Context, id StageID, to Path) error {
	if to.IsRoot() {
		return fmt.Errorf("%w: cannot write to the root", ErrInvalidPath)
	}
	if err := authorize(ctx, s.guard, OpWrite, to); err != nil {
		return err
	}
	if err := s.checkWritable(); err != nil {
		return err
	}

	p, err := s.stagePath(id)
	if err != nil {
		return err
	}
	info, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNoStagedFile
		}
		return fmt.Errorf("storage: s3 stage: %w", err)
	}

	if err := s.ensureDir(ctx, to.Parent()); err != nil {
		return err
	}
	if taken, err := s.exists(ctx, to); err != nil {
		return err
	} else if taken {
		return ErrExist
	}

	if info.Size() == 0 {
		if err := s.putEmpty(ctx, s.key(to), to.Name()); err != nil {
			return err
		}
	} else {
		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("storage: s3 stage: %w", err)
		}
		err = s.uploadStaged(ctx, f, info.Size(), to)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return fmt.Errorf("storage: s3 stage: %w", closeErr)
		}
	}

	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("storage: s3 stage: clear: %w", err)
	}
	s.metaStore(ctx, to, MetaEntry{Size: info.Size(), ModTime: time.Now()})
	s.metaInvalidateDir(ctx, to.Parent())
	return nil
}

// streams an open staged file into the bucket, utilizing PutObject or a multipart upload with minimal buffering.
func (s *S3) uploadStaged(ctx context.Context, src *os.File, size int64, to Path) error {
	key := s.key(to)
	if size <= s.partSize {
		_, err := s.client.PutObject(ctx, &awss3.PutObjectInput{
			Bucket:             aws.String(s.bucket),
			Key:                aws.String(key),
			Body:               io.NewSectionReader(src, 0, size),
			ContentType:        aws.String(contentTypeFor(to.Name())),
			ContentDisposition: aws.String(attachmentDisposition(to.Name())),
		})
		return mapS3Err(err)
	}

	create, err := s.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket:             aws.String(s.bucket),
		Key:                aws.String(key),
		ContentType:        aws.String(contentTypeFor(to.Name())),
		ContentDisposition: aws.String(attachmentDisposition(to.Name())),
	})
	if err != nil {
		return mapS3Err(err)
	}
	uploadID := aws.ToString(create.UploadId)
	completed := false
	defer func() {
		if !completed {
			_, _ = s.client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
				Bucket:   aws.String(s.bucket),
				Key:      aws.String(key),
				UploadId: aws.String(uploadID),
			})
		}
	}()

	parts := make([]types.CompletedPart, 0, (size+s.partSize-1)/s.partSize)
	for number, offset := int32(1), int64(0); offset < size; number, offset = number+1, offset+s.partSize {
		end := offset + s.partSize
		if end > size {
			end = size
		}
		part, err := s.client.UploadPart(ctx, &awss3.UploadPartInput{
			Bucket:     aws.String(s.bucket),
			Key:        aws.String(key),
			UploadId:   aws.String(uploadID),
			PartNumber: aws.Int32(number),
			Body:       io.NewSectionReader(src, offset, end-offset),
		})
		if err != nil {
			return mapS3Err(err)
		}
		parts = append(parts, types.CompletedPart{ETag: part.ETag, PartNumber: aws.Int32(number)})
	}

	_, err = s.client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err := mapS3Err(err); err != nil {
		return err
	}
	completed = true
	return nil
}

// OpenStage implements [Stager], so a checksum can be computed before the
// file is published.
func (s *S3) OpenStage(_ context.Context, id StageID) (File, error) {
	p, err := s.stagePath(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoStagedFile
		}
		return nil, fmt.Errorf("storage: s3 stage: %w", err)
	}
	return f, nil
}

// DiscardStage implements [Stager]. It is idempotent.
func (s *S3) DiscardStage(_ context.Context, id StageID) error {
	p, err := s.stagePath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("storage: s3 stage: %w", err)
	}
	return nil
}

// PurgeStagesOlderThan implements [Stager]: abandoned downloads are the one
// thing in the staging directory that grows without anyone asking.
func (s *S3) PurgeStagesOlderThan(_ context.Context, cutoff time.Time) (int, error) {
	entries, err := os.ReadDir(s.stageDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("storage: s3 stage: purge: %w", err)
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.stageDir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, fmt.Errorf("storage: s3 stage: purge: %w", err)
		}
		removed++
	}
	return removed, nil
}
