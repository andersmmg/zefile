package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// The S3 backend presents a bucket as the storage tree (AWS S3, Backblaze
// B2, Cloudflare R2, MinIO …). File data never passes through this
// process: uploads arrive through presigned part URLs and downloads leave
// through presigned GETs; only metadata calls are issued from here.

// S3Config configures an [S3].
type S3Config struct {
	// Bucket is the bucket holding the tree. Required.
	Bucket string

	// Region is the signing region. Required.
	Region string

	// Endpoint addresses a non-AWS provider, e.g.
	// https://s3.us-west-004.backblazeb2.com. Empty means AWS.
	Endpoint string

	// Prefix scopes the tree inside the bucket, e.g. "data". Slashes on
	// either side are ignored. Optional.
	Prefix string

	// AccessKeyID and SecretAccessKey are static credentials. When
	// AccessKeyID is empty the ambient AWS chain is used.
	AccessKeyID     string
	SecretAccessKey string

	// ForcePathStyle addresses buckets by path rather than by subdomain.
	ForcePathStyle bool

	// PublicEndpoint is the bucket's address as clients see it, used only to
	// mint presigned URLs. Empty means Endpoint.
	PublicEndpoint string

	// PartSize is the byte length of every multipart part except the last.
	// Non-positive means [DefaultPartSize].
	PartSize int64

	// PresignTTL bounds minted capability URLs. Non-positive means 15 min.
	PresignTTL time.Duration

	// Guard authorises operations. Nil means [AllowAll].
	Guard Guard

	// ReadOnly refuses every write regardless of anything else.
	ReadOnly bool

	// MetaCache mirrors object metadata so listings and stats skip
	// per-file round trips. Nil disables the cache; every operation
	// then talks to S3 directly. See [MetaCache].
	MetaCache MetaCache
}

// S3 is an [FS] backed by an S3-compatible object store.
type S3 struct {
	client     *awss3.Client
	presign    *awss3.PresignClient
	public     *awss3.PresignClient // nil when clients share the endpoint
	bucket     string
	prefix     string // "" or "data" — no leading or trailing slash
	guard      Guard
	readOnly   bool
	partSize   int64
	presignTTL time.Duration
	meta       MetaCache // nil means uncached
	// presigns reuses capability URLs within their validity window so
	// redirects to the store stay stable and the browser's HTTP cache can
	// answer repeated previews. See [presignCache].
	presigns presignCache
}

var _ FS = (*S3)(nil)

// OpenS3 confines an S3 to the given bucket and verifies it is reachable.
func OpenS3(cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("storage: s3 bucket is required")
	}
	if cfg.Region == "" {
		return nil, errors.New("storage: s3 region is required")
	}

	guard := cfg.Guard
	if guard == nil {
		guard = AllowAll{}
	}
	partSize := cfg.PartSize
	if partSize <= 0 {
		partSize = DefaultPartSize
	}
	presignTTL := cfg.PresignTTL
	if presignTTL <= 0 {
		presignTTL = 15 * time.Minute
	}

	client, err := newS3Client(cfg, cfg.Endpoint)
	if err != nil {
		return nil, err
	}

	s := &S3{
		client:     client,
		presign:    awss3.NewPresignClient(client),
		bucket:     cfg.Bucket,
		prefix:     strings.Trim(cfg.Prefix, "/"),
		guard:      guard,
		readOnly:   cfg.ReadOnly,
		partSize:   partSize,
		presignTTL: presignTTL,
		meta:       cfg.MetaCache,
	}
	if cfg.PublicEndpoint != "" && cfg.PublicEndpoint != cfg.Endpoint {
		// Capability URLs must resolve where the client runs.
		pub, err := newS3Client(cfg, cfg.PublicEndpoint)
		if err != nil {
			return nil, err
		}
		s.public = awss3.NewPresignClient(pub)
	}
	if _, err := client.HeadBucket(context.Background(), &awss3.HeadBucketInput{
		Bucket: aws.String(cfg.Bucket),
	}); err != nil {
		return nil, fmt.Errorf("storage: s3 bucket %q: %w", cfg.Bucket, mapS3Err(err))
	}
	return s, nil
}

// newS3Client builds the API client. An empty endpoint means AWS's own.
func newS3Client(cfg S3Config, endpoint string) (*awss3.Client, error) {
	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithRetryMode(aws.RetryModeStandard),
		awsconfig.WithRetryMaxAttempts(5),
	}
	if cfg.AccessKeyID != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("storage: s3 configuration: %w", err)
	}
	return awss3.NewFromConfig(awsCfg, func(o *awss3.Options) {
		o.UsePathStyle = cfg.ForcePathStyle
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	}), nil
}

func (s *S3) Close() error { return nil }

// ------------------------------------------------------------------ read path

// Stat implements [FS]. A fresh cache row answers without a round trip;
// otherwise one probe serves the answer and refreshes the row.
func (s *S3) Stat(ctx context.Context, p Path) (FileInfo, error) {
	if err := authorize(ctx, s.guard, OpRead, p); err != nil {
		return FileInfo{}, err
	}
	if p.IsRoot() {
		return FileInfo{Path: p, Name: p.Name(), IsDir: true}, nil
	}

	now := time.Now()
	if s.meta != nil {
		if e, err := s.meta.Hit(ctx, p, now); err == nil {
			return FileInfo{
				Path:    p,
				Name:    p.Name(),
				Size:    e.Size,
				ModTime: e.ModTime,
				IsDir:   e.IsDir,
			}, nil
		}
		// Any cache miss or error falls through to the store.
	}

	file, dir, err := s.probePath(ctx, p)
	if err != nil {
		return FileInfo{}, err
	}
	if file.found {
		s.metaStore(ctx, p, MetaEntry{Size: file.size, ModTime: file.modTime, CachedAt: now})
		return FileInfo{
			Path:    p,
			Name:    p.Name(),
			Size:    file.size,
			ModTime: file.modTime,
			IsDir:   false,
		}, nil
	}
	if !dir.found {
		// The store is truth: drop any row for a path deleted outside
		// the app so it cannot linger past this miss.
		s.metaForget(ctx, p)
		return FileInfo{}, ErrNotExist
	}
	s.metaStore(ctx, p, MetaEntry{ModTime: dir.modTime, IsDir: true, CachedAt: now})
	return FileInfo{Path: p, Name: p.Name(), ModTime: dir.modTime, IsDir: true}, nil
}

// List implements [FS]. A fresh cached listing answers without any round
// trip; otherwise one listing serves the answer and refreshes the cache.
// Cached rows are unfiltered — per-user permission filtering runs on every
// serve, exactly as for a live listing.
func (s *S3) List(ctx context.Context, p Path) ([]FileInfo, error) {
	traverse, err := authorizeList(ctx, s.guard, p)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	if s.meta != nil {
		if kids, err := s.meta.ChildrenHit(ctx, p, now); err == nil {
			return s.filterCached(ctx, p, kids, traverse)
		}
		// Any cache miss or error falls through to the store.
	}

	if !p.IsRoot() {
		file, dir, err := s.probePath(ctx, p)
		if err != nil {
			return nil, err
		}
		if file.found {
			return nil, ErrNotDir
		}
		if !dir.found {
			s.metaForget(ctx, p)
			return nil, ErrNotExist
		}
	}

	entries, err := s.listChildren(ctx, p)
	if err != nil {
		return nil, err
	}
	s.metaReplaceDir(ctx, p, entries, now)
	if traverse {
		return filterVisible(ctx, s.guard.(ListGuard), entries)
	}
	return filterReadable(ctx, s.guard, entries)
}

// filterCached serves a cached listing through the same permission filters
// as a live one. Map order is random, so entries are sorted by name for a
// stable order.
func (s *S3) filterCached(ctx context.Context, p Path, kids map[string]MetaEntry, traverse bool) ([]FileInfo, error) {
	names := make([]string, 0, len(kids))
	for name := range kids {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]FileInfo, 0, len(kids))
	for _, name := range names {
		e := kids[name]
		child, err := p.Child(name)
		if err != nil {
			continue
		}
		entries = append(entries, FileInfo{
			Path:    child,
			Name:    name,
			Size:    e.Size,
			ModTime: e.ModTime,
			IsDir:   e.IsDir,
		})
	}
	if traverse {
		return filterVisible(ctx, s.guard.(ListGuard), entries)
	}
	return filterReadable(ctx, s.guard, entries)
}

// The meta* helpers maintain the metadata cache best-effort: a cache write
// must never fail an operation the store itself accepted, so errors are
// dropped. A missed invalidation only costs a re-list — rows are never
// served past their TTL or without a fresh listing stamp.
func (s *S3) metaStore(ctx context.Context, p Path, e MetaEntry) {
	if s.meta == nil {
		return
	}
	if e.CachedAt.IsZero() {
		e.CachedAt = time.Now()
	}
	_ = s.meta.Store(ctx, p, e)
}

func (s *S3) metaReplaceDir(ctx context.Context, dir Path, entries []FileInfo, now time.Time) {
	if s.meta == nil {
		return
	}
	kids := make(map[string]MetaEntry, len(entries))
	for _, e := range entries {
		kids[e.Name] = MetaEntry{Size: e.Size, ModTime: e.ModTime, IsDir: e.IsDir, CachedAt: now}
	}
	_ = s.meta.ReplaceDir(ctx, dir, kids, now)
}

func (s *S3) metaForget(ctx context.Context, p Path) {
	if s.meta == nil {
		return
	}
	_ = s.meta.Forget(ctx, p)
}

func (s *S3) metaInvalidateDir(ctx context.Context, dir Path) {
	if s.meta == nil {
		return
	}
	_ = s.meta.InvalidateDir(ctx, dir)
}

// metaInvalidateAncestors drops the listing stamps from p up to the root,
// after an operation changed entries at several levels at once.
func (s *S3) metaInvalidateAncestors(ctx context.Context, p Path) {
	if s.meta == nil {
		return
	}
	for d := p; !d.IsRoot(); d = d.Parent() {
		_ = s.meta.InvalidateDir(ctx, d.Parent())
	}
}

func (s *S3) metaRelocate(ctx context.Context, from, to Path) {
	if s.meta == nil {
		return
	}
	_ = s.meta.Relocate(ctx, from, to)
}

// listChildren lists one directory level, following pagination.
func (s *S3) listChildren(ctx context.Context, p Path) ([]FileInfo, error) {
	prefix := s.dirPrefix(p)
	var token *string
	entries := make([]FileInfo, 0, 16)

	for {
		out, err := s.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			Delimiter:         aws.String("/"),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, mapS3Err(err)
		}

		for _, obj := range out.Contents {
			key := aws.ToString(obj.Key)
			if key == prefix {
				continue
			}
			name := strings.TrimPrefix(key, prefix)
			if name == "" || strings.Contains(name, "/") {
				continue
			}
			if p.IsRoot() && isReservedName(name) {
				continue
			}
			child, err := p.Child(name)
			if err != nil {
				continue
			}
			entries = append(entries, FileInfo{
				Path:    child,
				Name:    name,
				Size:    aws.ToInt64(obj.Size),
				ModTime: timeOrZero(obj.LastModified),
			})
		}

		for _, cp := range out.CommonPrefixes {
			key := aws.ToString(cp.Prefix)
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(key, prefix), "/")
			if name == "" {
				continue
			}
			if p.IsRoot() && isReservedName(name) {
				continue
			}
			child, err := p.Child(name)
			if err != nil {
				continue
			}
			// S3 carries no modification time for a prefix.
			entries = append(entries, FileInfo{Path: child, Name: name, IsDir: true})
		}

		if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			break
		}
		token = out.NextContinuationToken
	}
	return entries, nil
}

// ----------------------------------------------------------------- write path

// Mkdir implements [FS]. Parents must already exist.
func (s *S3) Mkdir(ctx context.Context, p Path) error {
	if p.IsRoot() {
		return ErrExist
	}
	if err := authorize(ctx, s.guard, OpWrite, p); err != nil {
		return err
	}
	if err := s.checkWritable(); err != nil {
		return err
	}
	if err := s.ensureDir(ctx, p.Parent()); err != nil {
		return err
	}
	taken, err := s.exists(ctx, p)
	if err != nil {
		return err
	}
	if taken {
		return ErrExist
	}
	if err := s.putMarker(ctx, s.dirPrefix(p)); err != nil {
		return err
	}
	s.metaStore(ctx, p, MetaEntry{ModTime: time.Now(), IsDir: true})
	s.metaInvalidateDir(ctx, p.Parent())
	return nil
}

// MkdirAll implements [FS], creating missing ancestors as markers.
func (s *S3) MkdirAll(ctx context.Context, p Path) error {
	if p.IsRoot() {
		return nil
	}
	if err := authorize(ctx, s.guard, OpWrite, p); err != nil {
		return err
	}
	if err := s.checkWritable(); err != nil {
		return err
	}

	key := s.prefix
	for _, comp := range p.components() {
		if key == "" {
			key = comp
		} else {
			key = key + "/" + comp
		}

		out, err := s.head(ctx, key)
		if err != nil {
			return err
		}
		if out != nil {
			return ErrNotDir // a file stands where a directory is needed
		}
		exists, _, err := s.probeDir(ctx, key+"/")
		if err != nil {
			return err
		}
		if !exists {
			if err := s.putMarker(ctx, key+"/"); err != nil {
				return err
			}
		}
	}
	s.metaStore(ctx, p, MetaEntry{ModTime: time.Now(), IsDir: true})
	s.metaInvalidateAncestors(ctx, p)
	return nil
}

// Space implements [FS]. A bucket has no statfs: only the configured
// read-only flag refuses writes.
func (s *S3) Space(context.Context) (SpaceInfo, error) {
	return SpaceInfo{ReadOnly: s.readOnly}, nil
}

// -------------------------------------------------------- unimplemented parts

// Create implements [FS]. Refused: uploads go through [DirectStager],
// never as bytes through this process.
func (s *S3) Create(context.Context, Path) (WriteFile, error) {
	return nil, fmt.Errorf("%w: create", ErrNotSupported)
}

// Open lives in s3_open.go: ranged server-side reads.

// Copy, Move, Remove and RemoveAll live in s3_copy.go.

// -------------------------------------------------------------------- helpers

// key maps a path to its object key.
func (s *S3) key(p Path) string {
	rel := strings.Join(p.components(), "/")
	if s.prefix == "" {
		return rel
	}
	if rel == "" {
		return s.prefix
	}
	return s.prefix + "/" + rel
}

// dirPrefix is the key a directory's children live under.
func (s *S3) dirPrefix(p Path) string {
	k := s.key(p)
	if k == "" {
		return ""
	}
	return k + "/"
}

// head looks an object up. A missing object answers (nil, nil).
func (s *S3) head(ctx context.Context, key string) (*awss3.HeadObjectOutput, error) {
	out, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, mapS3Err(err)
	}
	return out, nil
}

// probeDir reports whether anything exists under a directory key, and the
// marker's modification time when it is the marker that exists.
func (s *S3) probeDir(ctx context.Context, dirKey string) (exists bool, modTime time.Time, err error) {
	if dirKey == "" {
		return true, time.Time{}, nil
	}
	out, err := s.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		Prefix:  aws.String(dirKey),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return false, time.Time{}, mapS3Err(err)
	}
	if len(out.Contents) == 0 {
		return false, time.Time{}, nil
	}
	if aws.ToString(out.Contents[0].Key) == dirKey {
		return true, timeOrZero(out.Contents[0].LastModified), nil
	}
	return true, time.Time{}, nil
}

type probedObject struct {
	found   bool
	size    int64
	modTime time.Time
}

func (s *S3) probePath(ctx context.Context, p Path) (file, dir probedObject, err error) {
	if p.IsRoot() {
		return probedObject{}, probedObject{found: true}, nil
	}
	key, dirKey := s.key(p), s.dirPrefix(p)
	out, err := s.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		Prefix:  aws.String(key),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return probedObject{}, probedObject{}, mapS3Err(err)
	}
	if len(out.Contents) == 0 {
		return probedObject{}, probedObject{}, nil
	}
	obj := out.Contents[0]
	switch k := aws.ToString(obj.Key); {
	case k == key:
		file = probedObject{found: true, size: aws.ToInt64(obj.Size), modTime: timeOrZero(obj.LastModified)}
	case k == dirKey:
		dir = probedObject{found: true, modTime: timeOrZero(obj.LastModified)}
	case strings.HasPrefix(k, dirKey):
		dir = probedObject{found: true}
	}
	return file, dir, nil
}

// exists reports whether anything already occupies a path.
func (s *S3) exists(ctx context.Context, p Path) (bool, error) {
	file, dir, err := s.probePath(ctx, p)
	if err != nil {
		return false, err
	}
	return file.found || dir.found, nil
}

// ensureDir requires p to be a directory.
func (s *S3) ensureDir(ctx context.Context, p Path) error {
	if p.IsRoot() {
		return nil
	}
	file, dir, err := s.probePath(ctx, p)
	if err != nil {
		return err
	}
	if file.found {
		return ErrNotDir
	}
	if !dir.found {
		return ErrNotExist
	}
	return nil
}

// putMarker writes a zero-byte object under the directory key. Callers
// check existence first: providers differ on conditional writes, so the
// check-then-put race stands, as with a concurrent mkdir.
func (s *S3) putMarker(ctx context.Context, dirKey string) error {
	_, err := s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(dirKey),
		Body:   bytes.NewReader(nil),
	})
	return mapS3Err(err)
}

// checkWritable refuses writes when the instance is configured read-only.
func (s *S3) checkWritable() error {
	if s.readOnly {
		return ErrReadOnly
	}
	return nil
}

// isNotFound reports whether a provider error means the object is absent.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NoSuchBucket", "NotFound", "404":
			return true
		}
	}
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) && status.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	return errors.Is(err, fs.ErrNotExist)
}

// mapS3Err translates provider errors into this package's sentinels.
// Unrecognised errors pass through unchanged.
func mapS3Err(err error) error {
	if err == nil {
		return nil
	}
	if isNotFound(err) {
		return ErrNotExist
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "AccessDenied", "AllAccessDisabled", "SignatureDoesNotMatch",
			"InvalidAccessKeyId", "ExpiredToken":
			return ErrPermission
		case "Conflict", "PreconditionFailed",
			"BucketAlreadyExists", "BucketAlreadyOwnedByYou":
			return ErrExist
		}
	}
	return err
}

func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}
