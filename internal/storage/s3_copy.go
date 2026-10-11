package storage

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

const copyParallelism = 8

// Server-side copies, moves and deletes. There is no rename in object
// storage, so a move is a copy followed by a delete of the originals:
// not atomic (a failure between the two leaves both), never clobbering
// (an existing destination is refused), and verified before any delete.

var _ TreeCopier = (*S3)(nil)

type listedObject struct {
	key  string
	size int64
}

func (s *S3) listAllObjects(ctx context.Context, prefix string) ([]listedObject, error) {
	var out []listedObject
	var token *string
	for {
		resp, err := s.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, mapS3Err(err)
		}
		for _, obj := range resp.Contents {
			out = append(out, listedObject{key: aws.ToString(obj.Key), size: aws.ToInt64(obj.Size)})
		}
		if !aws.ToBool(resp.IsTruncated) || resp.NextContinuationToken == nil {
			return out, nil
		}
		token = resp.NextContinuationToken
	}
}

// verifyCopy reads the copied object back and checks its size before the
// source may be deleted.
func (s *S3) verifyCopy(ctx context.Context, dstKey string, want int64) error {
	out, err := s.head(ctx, dstKey)
	if err != nil {
		return err
	}
	if out == nil {
		return fmt.Errorf("storage: server-side copy to %q is not visible yet", dstKey)
	}
	if got := aws.ToInt64(out.ContentLength); got != want {
		return fmt.Errorf("storage: server-side copy to %q holds %d bytes, want %d", dstKey, got, want)
	}
	return nil
}

// Copy implements [FS]. Directories and files past [MaxSyncCopyBytes] are
// refused as on Local; both go through [TreeCopier] in the job queue.
func (s *S3) Copy(ctx context.Context, from, to Path) error {
	if from.IsRoot() || to.IsRoot() {
		return fmt.Errorf("%w: cannot copy the root", ErrInvalidPath)
	}
	if err := authorize(ctx, s.guard, OpRead, from); err != nil {
		return err
	}
	if err := authorize(ctx, s.guard, OpWrite, to); err != nil {
		return err
	}
	if err := s.checkWritable(); err != nil {
		return err
	}

	info, err := s.Stat(ctx, from)
	if err != nil {
		return err
	}
	if info.IsDir {
		return fmt.Errorf("%w: copying a directory needs the background job queue", ErrIsDir)
	}
	if info.Size > MaxSyncCopyBytes {
		return fmt.Errorf("%w: %d bytes exceeds the %d byte limit", ErrTooLarge, info.Size, MaxSyncCopyBytes)
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

	if err := s.copyKey(ctx, s.key(from), s.key(to)); err != nil {
		return err
	}
	if err := s.verifyCopy(ctx, s.key(to), info.Size); err != nil {
		return err
	}
	s.metaStore(ctx, to, MetaEntry{Size: info.Size, ModTime: info.ModTime})
	s.metaInvalidateDir(ctx, to.Parent())
	return nil
}

// CopyTree implements [TreeCopier]. It runs in the job worker, never in a
// request. A failure partway leaves a partial tree at the destination:
// there is no atomic commit without a rename.
func (s *S3) CopyTree(ctx context.Context, from, to Path, progress func(done, total int64)) error {
	if from.IsRoot() || to.IsRoot() {
		return fmt.Errorf("%w: cannot copy the root", ErrInvalidPath)
	}
	if err := authorize(ctx, s.guard, OpRead, from); err != nil {
		return err
	}
	if err := authorize(ctx, s.guard, OpWrite, to); err != nil {
		return err
	}
	if err := s.checkWritable(); err != nil {
		return err
	}

	info, err := s.Stat(ctx, from)
	if err != nil {
		return err
	}
	taken, err := s.exists(ctx, to)
	if err != nil {
		return err
	}
	if taken {
		return ErrExist
	}
	if err := s.ensureDir(ctx, to.Parent()); err != nil {
		return err
	}

	report := func(done, total int64) {
		if progress != nil {
			progress(done, total)
		}
	}

	if !info.IsDir {
		if err := s.copyKey(ctx, s.key(from), s.key(to)); err != nil {
			return err
		}
		if err := s.verifyCopy(ctx, s.key(to), info.Size); err != nil {
			return err
		}
		s.metaStore(ctx, to, MetaEntry{Size: info.Size, ModTime: info.ModTime})
		s.metaInvalidateDir(ctx, to.Parent())
		report(info.Size, info.Size)
		return nil
	}

	srcPrefix, dstPrefix := s.dirPrefix(from), s.dirPrefix(to)
	objects, err := s.listAllObjects(ctx, srcPrefix)
	if err != nil {
		return err
	}
	if len(objects) == 0 {
		return s.putMarker(ctx, dstPrefix)
	}

	want := make(map[string]struct{}, len(objects))
	for _, obj := range objects {
		want[strings.TrimPrefix(obj.key, srcPrefix)] = struct{}{}
	}
	if err := s.copyObjects(ctx, objects, srcPrefix, dstPrefix, report); err != nil {
		return err
	}
	if err := s.verifyTree(ctx, dstPrefix, want); err != nil {
		return err
	}
	s.metaStore(ctx, to, MetaEntry{ModTime: time.Now(), IsDir: true})
	s.metaInvalidateAncestors(ctx, to)
	return nil
}

func (s *S3) copyObjects(ctx context.Context, objects []listedObject, srcPrefix, dstPrefix string, report func(done, total int64)) error {
	var total int64
	for _, obj := range objects {
		total += obj.size
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan listedObject)
	var done atomic.Int64
	var once sync.Once
	var firstErr atomic.Value // error
	var mu sync.Mutex

	worker := func() {
		for obj := range jobs {
			rel := strings.TrimPrefix(obj.key, srcPrefix)
			if err := s.copyKeySized(ctx, obj.key, dstPrefix+rel, obj.size); err != nil {
				once.Do(func() { firstErr.Store(err) })
				cancel()
				return
			}
			if report != nil {
				mu.Lock()
				report(done.Add(obj.size), total)
				mu.Unlock()
			}
		}
	}

	var wg sync.WaitGroup
	for range copyParallelism {
		wg.Add(1)
		go func() { defer wg.Done(); worker() }()
	}
	for _, obj := range objects {
		select {
		case jobs <- obj:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()

	if err, ok := firstErr.Load().(error); ok && err != nil {
		return err
	}
	return ctx.Err()
}

// verifyTree checks the destination holds exactly the copied set.
func (s *S3) verifyTree(ctx context.Context, dstPrefix string, want map[string]struct{}) error {
	got, err := s.listAllObjects(ctx, dstPrefix)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(got))
	for _, obj := range got {
		seen[strings.TrimPrefix(obj.key, dstPrefix)] = struct{}{}
	}
	if len(seen) != len(want) {
		return fmt.Errorf("storage: server-side tree copy landed %d objects, want %d", len(seen), len(want))
	}
	for rel := range want {
		if _, ok := seen[rel]; !ok {
			return fmt.Errorf("storage: server-side tree copy is missing %q", rel)
		}
	}
	return nil
}

// Move implements [FS] as a server-side copy followed by a delete. It
// never overwrites: without an atomic rename a replace cannot be safe.
func (s *S3) Move(ctx context.Context, from, to Path) error {
	if from.IsRoot() || to.IsRoot() {
		return fmt.Errorf("%w: cannot move the root", ErrInvalidPath)
	}
	if err := authorize(ctx, s.guard, OpDelete, from); err != nil {
		return err
	}
	if err := authorize(ctx, s.guard, OpWrite, to); err != nil {
		return err
	}
	if err := s.checkWritable(); err != nil {
		return err
	}
	if strings.HasPrefix(to.String()+"/", from.String()+"/") {
		return fmt.Errorf("%w: cannot move a directory into itself", ErrInvalidPath)
	}

	info, err := s.Stat(ctx, from)
	if err != nil {
		return err
	}
	taken, err := s.exists(ctx, to)
	if err != nil {
		return err
	}
	if taken {
		return ErrExist
	}
	if err := s.ensureDir(ctx, to.Parent()); err != nil {
		return err
	}

	if !info.IsDir {
		if err := s.copyKey(ctx, s.key(from), s.key(to)); err != nil {
			return err
		}
		if err := s.verifyCopy(ctx, s.key(to), info.Size); err != nil {
			return err
		}
		_, err = s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(s.key(from)),
		})
		if err := mapS3Err(err); err != nil {
			return err
		}
		s.metaRelocate(ctx, from, to)
		return nil
	}

	srcPrefix, dstPrefix := s.dirPrefix(from), s.dirPrefix(to)
	objects, err := s.listAllObjects(ctx, srcPrefix)
	if err != nil {
		return err
	}
	if len(objects) == 0 {
		if err := s.putMarker(ctx, dstPrefix); err != nil {
			return err
		}
		s.metaRelocate(ctx, from, to)
		return nil
	}
	srcKeys := make([]string, 0, len(objects))
	want := make(map[string]struct{}, len(objects))
	for _, obj := range objects {
		srcKeys = append(srcKeys, obj.key)
		want[strings.TrimPrefix(obj.key, srcPrefix)] = struct{}{}
	}
	if err := s.copyObjects(ctx, objects, srcPrefix, dstPrefix, nil); err != nil {
		return err
	}
	if err := s.verifyTree(ctx, dstPrefix, want); err != nil {
		return err
	}
	if err := s.deleteKeys(ctx, srcKeys); err != nil {
		return err
	}
	s.metaRelocate(ctx, from, to)
	return nil
}

// Remove implements [FS]. A directory must be empty apart from its marker.
func (s *S3) Remove(ctx context.Context, p Path) error {
	if p.IsRoot() {
		return fmt.Errorf("%w: cannot remove the root", ErrInvalidPath)
	}
	if err := authorize(ctx, s.guard, OpDelete, p); err != nil {
		return err
	}
	if err := s.checkWritable(); err != nil {
		return err
	}

	info, err := s.Stat(ctx, p)
	if err != nil {
		return err
	}
	if !info.IsDir {
		_, err = s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(s.key(p)),
		})
		if err := mapS3Err(err); err != nil {
			return err
		}
		s.metaForget(ctx, p)
		s.metaInvalidateDir(ctx, p.Parent())
		return nil
	}

	objects, err := s.listAllObjects(ctx, s.dirPrefix(p))
	if err != nil {
		return err
	}
	for _, obj := range objects {
		if obj.key != s.dirPrefix(p) {
			return ErrNotEmpty
		}
	}
	if err := s.deleteKeys(ctx, []string{s.dirPrefix(p)}); err != nil {
		return err
	}
	s.metaForget(ctx, p)
	s.metaInvalidateDir(ctx, p.Parent())
	return nil
}

// RemoveAll implements [FS]. It is idempotent, like os.RemoveAll.
func (s *S3) RemoveAll(ctx context.Context, p Path) error {
	if p.IsRoot() {
		return fmt.Errorf("%w: cannot remove the root", ErrInvalidPath)
	}
	if err := authorize(ctx, s.guard, OpDelete, p); err != nil {
		return err
	}
	if err := s.checkWritable(); err != nil {
		return err
	}

	// Match only the key itself and its directory prefix, not siblings
	// sharing the name's spelling ("docs" vs "docs-draft").
	key, prefix := s.key(p), s.dirPrefix(p)
	objects, err := s.listAllObjects(ctx, key)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(objects))
	for _, obj := range objects {
		if obj.key == key || strings.HasPrefix(obj.key, prefix) {
			keys = append(keys, obj.key)
		}
	}
	if err := s.deleteKeys(ctx, keys); err != nil {
		return err
	}
	s.metaForget(ctx, p)
	s.metaInvalidateDir(ctx, p.Parent())
	return nil
}
