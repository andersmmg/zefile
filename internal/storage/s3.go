package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
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

// Stat implements [FS].
func (s *S3) Stat(ctx context.Context, p Path) (FileInfo, error) {
	if err := authorize(ctx, s.guard, OpRead, p); err != nil {
		return FileInfo{}, err
	}
	if p.IsRoot() {
		return FileInfo{Path: p, Name: p.Name(), IsDir: true}, nil
	}

	out, err := s.head(ctx, s.key(p))
	if err != nil {
		return FileInfo{}, err
	}
	if out != nil {
		return FileInfo{
			Path:    p,
			Name:    p.Name(),
			Size:    aws.ToInt64(out.ContentLength),
			ModTime: timeOrZero(out.LastModified),
			IsDir:   false,
		}, nil
	}

	exists, modTime, err := s.probeDir(ctx, s.dirPrefix(p))
	if err != nil {
		return FileInfo{}, err
	}
	if !exists {
		return FileInfo{}, ErrNotExist
	}
	return FileInfo{Path: p, Name: p.Name(), ModTime: modTime, IsDir: true}, nil
}

// List implements [FS].
func (s *S3) List(ctx context.Context, p Path) ([]FileInfo, error) {
	traverse, err := authorizeList(ctx, s.guard, p)
	if err != nil {
		return nil, err
	}

	if !p.IsRoot() {
		out, err := s.head(ctx, s.key(p))
		if err != nil {
			return nil, err
		}
		if out != nil {
			return nil, ErrNotDir
		}
		exists, _, err := s.probeDir(ctx, s.dirPrefix(p))
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, ErrNotExist
		}
	}

	entries, err := s.listChildren(ctx, p)
	if err != nil {
		return nil, err
	}
	if traverse {
		return filterVisible(ctx, s.guard.(ListGuard), entries)
	}
	return filterReadable(ctx, s.guard, entries)
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
	return s.putMarker(ctx, s.dirPrefix(p))
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

// exists reports whether anything already occupies a path.
func (s *S3) exists(ctx context.Context, p Path) (bool, error) {
	out, err := s.head(ctx, s.key(p))
	if err != nil {
		return false, err
	}
	if out != nil {
		return true, nil
	}
	exists, _, err := s.probeDir(ctx, s.dirPrefix(p))
	return exists, err
}

// ensureDir requires p to be a directory.
func (s *S3) ensureDir(ctx context.Context, p Path) error {
	if p.IsRoot() {
		return nil
	}
	out, err := s.head(ctx, s.key(p))
	if err != nil {
		return err
	}
	if out != nil {
		return ErrNotDir
	}
	exists, _, err := s.probeDir(ctx, s.dirPrefix(p))
	if err != nil {
		return err
	}
	if !exists {
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
