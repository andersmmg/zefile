// Package config reads and validates the settings a Zefile instance runs on.
//
// Everything comes from the environment. A self-hosted service is deployed from
// a compose file far more often than from a configuration file, and one source
// of truth avoids the question of which wins.
//
// Validation is strict and happens at startup: a misconfigured instance should
// refuse to start with a clear message rather than run and fail later in a way
// that looks like a bug.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/krishna2206/zefile/internal/storage"
)

// Environment variables read by [Load].
const (
	EnvAppURL     = "ZEFILE_APP_URL"
	EnvContentURL = "ZEFILE_CONTENT_URL"
	EnvDataDir    = "ZEFILE_DATA_DIR"
	EnvConfigDir  = "ZEFILE_CONFIG_DIR"
	EnvListen     = "ZEFILE_LISTEN"
	EnvReserve    = "ZEFILE_RESERVE_BYTES"
	EnvReadOnly   = "ZEFILE_READ_ONLY"

	// Backend and object-storage settings
	EnvThumbMaxBytes     = "ZEFILE_THUMB_MAX_BYTES"
	EnvPreviewMaxBytes   = "ZEFILE_PREVIEW_MAX_BYTES"
	EnvQuotaBytes        = "ZEFILE_QUOTA_BYTES"
	EnvStorageBackend    = "ZEFILE_STORAGE_BACKEND"
	EnvS3Endpoint        = "ZEFILE_S3_ENDPOINT"
	EnvS3Region          = "ZEFILE_S3_REGION"
	EnvS3Bucket          = "ZEFILE_S3_BUCKET"
	EnvS3Prefix          = "ZEFILE_S3_PREFIX"
	EnvS3AccessKeyID     = "ZEFILE_S3_ACCESS_KEY_ID"
	EnvS3SecretAccessKey = "ZEFILE_S3_SECRET_ACCESS_KEY"
	EnvS3ForcePathStyle  = "ZEFILE_S3_FORCE_PATH_STYLE"
	EnvS3PartSize        = "ZEFILE_S3_PART_SIZE"
	EnvS3PresignTTL      = "ZEFILE_S3_PRESIGN_TTL"
	EnvS3MetaTTL         = "ZEFILE_S3_META_TTL"
	EnvS3PublicEndpoint  = "ZEFILE_S3_PUBLIC_ENDPOINT"
)

// Storage backends [Config.Backend] may name.
const (
	// BackendLocal stores files in a directory on the host. Default.
	BackendLocal = "local"

	// BackendS3 stores files in an S3-compatible bucket
	BackendS3 = "s3"
)

// DefaultListen is the address served when none is given.
const DefaultListen = ":8080"

const (
	// DefaultS3PartSize is the multipart part length when
	// ZEFILE_S3_PART_SIZE is unset.
	DefaultS3PartSize = storage.DefaultPartSize

	// minS3PartSize is the provider-side floor every part but the last must satisfy.
	minS3PartSize = 5 << 20

	// DefaultS3PresignTTL is how long minted capability URLs stay valid when
	// ZEFILE_S3_PRESIGN_TTL is unset.
	DefaultS3PresignTTL = 15 * time.Minute

	// DefaultS3MetaTTL is how long S3 metadata rows are served from the
	// cache when ZEFILE_S3_META_TTL is unset. Mutations the app makes
	// invalidate immediately; the TTL only bounds how stale rows for
	// changes made outside the app can get.
	DefaultS3MetaTTL = time.Minute

	// DefaultPreviewMaxBytes applies when ZEFILE_PREVIEW_MAX_BYTES is
	// unset. It mirrors the api default.
	DefaultPreviewMaxBytes = 2 << 20

	// DefaultThumbMaxBytes caps the source file a background thumbnail
	// job will fetch: a video can be gigabytes, and one frame is not
	// worth moving all of it.
	DefaultThumbMaxBytes = 512 << 20
)

// Config is a validated instance configuration.
type Config struct {
	// AppURL is the public address of the application origin, used to build
	// links and to decide whether cookies may be marked Secure.
	AppURL *url.URL

	// ContentURL is the public address of the content origin. When empty the
	// instance runs in single-origin mode, described in the design document as
	// a deliberate, hardened degradation rather than an accident.
	ContentURL *url.URL

	// DataDir is the storage root: the directory users browse.
	// It is not required — and not read — when Backend is [BackendS3].
	DataDir string

	// Backend selects the storage implementation.
	Backend string

	// S3 holds the object-storage settings (only for [BackendS3]).
	S3 S3Settings

	// ConfigDir holds the database. It must not be inside DataDir, which
	// [db.Open] enforces.
	ConfigDir string

	// Listen is the address the HTTP server binds.
	Listen string

	// Reserve is the free space kept in hand before refusing writes.
	Reserve uint64

	// ReadOnly refuses every write regardless of free space.
	ReadOnly bool

	// PreviewMaxBytes bounds server-side preview reads.
	PreviewMaxBytes int64

	// ThumbMaxBytes caps the source file a thumbnail job will fetch.
	ThumbMaxBytes int64

	// QuotaBytes caps instance bytes. Zero means unlimited.
	QuotaBytes int64
}

// SingleOrigin reports whether user content is served from the application
// origin, which requires the hardening described in the design document.
func (c Config) SingleOrigin() bool { return c.ContentURL == nil }

// S3Settings are the object-storage knobs, validated only in [BackendS3]
// mode. Empty credentials fall through to the ambient AWS chain.
type S3Settings struct {
	// Endpoint addresses a non-AWS provider, e.g.
	// https://s3.us-west-004.backblazeb2.com. Empty means AWS.
	Endpoint string

	// Region is required.
	Region string

	// Bucket holds the tree. Required.
	Bucket string

	// Prefix scopes the tree inside the bucket, e.g. "data/". Optional.
	Prefix string

	// AccessKeyID and SecretAccessKey are static credentials. Optional.
	AccessKeyID     string
	SecretAccessKey string

	// ForcePathStyle addresses buckets by path rather than by subdomain.
	ForcePathStyle bool

	// PublicEndpoint is the bucket's address as clients see it, used only to
	// mint presigned URLs. Empty means Endpoint.
	PublicEndpoint string

	// PartSize is the byte length of every multipart part except the last.
	PartSize int64

	// PresignTTL is how long minted capability URLs stay valid.
	PresignTTL time.Duration

	// MetaTTL bounds cached S3 metadata. Zero disables expiry-based
	// revalidation (mutations still invalidate). Negative is rejected.
	MetaTTL time.Duration
}

// SecureCookies reports whether session cookies may carry the Secure attribute.
//
// It follows the scheme of the public address rather than being a setting of
// its own. A Secure cookie sent over plain HTTP is simply discarded by the
// browser, which presents as an unexplained inability to sign in — deriving it
// removes a way to misconfigure the instance into that state.
func (c Config) SecureCookies() bool { return c.AppURL.Scheme == "https" }

// Load reads the configuration from the environment.
func Load() (Config, error) {
	cfg := Config{
		DataDir:         os.Getenv(EnvDataDir),
		ConfigDir:       os.Getenv(EnvConfigDir),
		Listen:          valueOr(os.Getenv(EnvListen), DefaultListen),
		Reserve:         storage.DefaultReserve,
		PreviewMaxBytes: DefaultPreviewMaxBytes,
		ThumbMaxBytes:   DefaultThumbMaxBytes,
	}

	var err error
	if cfg.AppURL, err = requiredURL(EnvAppURL); err != nil {
		return Config{}, err
	}
	if raw := os.Getenv(EnvContentURL); raw != "" {
		if cfg.ContentURL, err = parseURL(EnvContentURL, raw); err != nil {
			return Config{}, err
		}
		if cfg.ContentURL.Host == cfg.AppURL.Host {
			return Config{}, fmt.Errorf(
				"config: %s and %s have the same host %q; separate origins are what keep an uploaded file from reaching a session",
				EnvAppURL, EnvContentURL, cfg.AppURL.Host)
		}
	}

	if cfg.ConfigDir == "" {
		return Config{}, fmt.Errorf("config: %s is required", EnvConfigDir)
	}

	if cfg.Backend, err = StorageBackend(); err != nil {
		return Config{}, err
	}
	if cfg.Backend == BackendS3 {
		if cfg.S3, err = loadS3(); err != nil {
			return Config{}, err
		}
		cfg.DataDir = ""
	} else {
		if cfg.DataDir == "" {
			return Config{}, fmt.Errorf("config: %s is required", EnvDataDir)
		}
		if err := mustBeDirectory(EnvDataDir, cfg.DataDir); err != nil {
			return Config{}, err
		}
	}

	if raw := os.Getenv(EnvReserve); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("config: %s must be a number of bytes: %w", EnvReserve, err)
		}
		cfg.Reserve = n
	}
	if raw := os.Getenv(EnvReadOnly); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("config: %s must be true or false: %w", EnvReadOnly, err)
		}
		cfg.ReadOnly = b
	}
	if raw := strings.TrimSpace(os.Getenv(EnvPreviewMaxBytes)); raw != "" {
		n, err := parseBytes(EnvPreviewMaxBytes, raw)
		if err != nil {
			return Config{}, err
		}
		cfg.PreviewMaxBytes = n
	}
	if raw := strings.TrimSpace(os.Getenv(EnvThumbMaxBytes)); raw != "" {
		n, err := parseBytes(EnvThumbMaxBytes, raw)
		if err != nil {
			return Config{}, err
		}
		cfg.ThumbMaxBytes = n
	}
	if raw := strings.TrimSpace(os.Getenv(EnvQuotaBytes)); raw != "" {
		n, err := parseBytes(EnvQuotaBytes, raw)
		if err != nil {
			return Config{}, err
		}
		if n < 0 {
			return Config{}, fmt.Errorf("config: %s must not be negative", EnvQuotaBytes)
		}
		cfg.QuotaBytes = n
	}

	return cfg, nil
}

// StorageBackend reads ZEFILE_STORAGE_BACKEND without the rest of [Load],
// for maintenance commands that skip full configuration.
func StorageBackend() (string, error) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(EnvStorageBackend)))
	switch raw {
	case "":
		return BackendLocal, nil
	case BackendLocal, BackendS3:
		return raw, nil
	default:
		return "", fmt.Errorf("config: %s must be %q or %q, got %q",
			EnvStorageBackend, BackendLocal, BackendS3, raw)
	}
}

// loadS3 reads and validates the object-storage settings at startup.
func loadS3() (S3Settings, error) {
	s := S3Settings{PartSize: DefaultS3PartSize, PresignTTL: DefaultS3PresignTTL, MetaTTL: DefaultS3MetaTTL}
	s.Endpoint = strings.TrimSpace(os.Getenv(EnvS3Endpoint))
	s.Region = strings.TrimSpace(os.Getenv(EnvS3Region))
	s.Bucket = strings.TrimSpace(os.Getenv(EnvS3Bucket))
	s.Prefix = strings.Trim(strings.TrimSpace(os.Getenv(EnvS3Prefix)), "/")
	s.AccessKeyID = os.Getenv(EnvS3AccessKeyID)
	s.SecretAccessKey = os.Getenv(EnvS3SecretAccessKey)
	s.PublicEndpoint = strings.TrimSpace(os.Getenv(EnvS3PublicEndpoint))

	if s.Bucket == "" {
		return S3Settings{}, fmt.Errorf("config: %s is required when %s is %q",
			EnvS3Bucket, EnvStorageBackend, BackendS3)
	}
	if s.Region == "" {
		return S3Settings{}, fmt.Errorf("config: %s is required when %s is %q",
			EnvS3Region, EnvStorageBackend, BackendS3)
	}
	for _, ep := range []struct {
		key, value string
	}{
		{EnvS3Endpoint, s.Endpoint},
		{EnvS3PublicEndpoint, s.PublicEndpoint},
	} {
		if ep.value == "" {
			continue
		}
		u, err := url.Parse(ep.value)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return S3Settings{}, fmt.Errorf("config: %s must be an http(s) URL, got %q",
				ep.key, ep.value)
		}
	}
	if s.AccessKeyID != "" && s.SecretAccessKey == "" {
		return S3Settings{}, fmt.Errorf("config: %s is set but %s is empty",
			EnvS3AccessKeyID, EnvS3SecretAccessKey)
	}
	if raw := os.Getenv(EnvS3ForcePathStyle); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return S3Settings{}, fmt.Errorf("config: %s must be true or false: %w", EnvS3ForcePathStyle, err)
		}
		s.ForcePathStyle = b
	}
	// Part sizes are bytes or KiB/MiB/GiB-suffixed, floored at the S3
	// multipart minimum.
	if raw := strings.TrimSpace(os.Getenv(EnvS3PartSize)); raw != "" {
		n, err := parseBytes(EnvS3PartSize, raw)
		if err != nil {
			return S3Settings{}, err
		}
		if n < minS3PartSize {
			return S3Settings{}, fmt.Errorf("config: %s must be at least %d (5 MiB), got %d",
				EnvS3PartSize, minS3PartSize, n)
		}
		s.PartSize = n
	}
	if raw := strings.TrimSpace(os.Getenv(EnvS3PresignTTL)); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return S3Settings{}, fmt.Errorf("config: %s must be a positive duration like 15m, got %q",
				EnvS3PresignTTL, raw)
		}
		s.PresignTTL = d
	}
	if raw := strings.TrimSpace(os.Getenv(EnvS3MetaTTL)); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return S3Settings{}, fmt.Errorf("config: %s must be a duration like 1m (0 disables expiry), got %q",
				EnvS3MetaTTL, raw)
		}
		s.MetaTTL = d
	}
	return s, nil
}

// parseBytes reads a byte count with an optional KiB/MiB/GiB suffix.
func parseBytes(key, raw string) (int64, error) {
	upper := strings.ToUpper(strings.TrimSpace(raw))
	mult := int64(1)
	for _, suf := range []struct {
		suffix string
		mult   int64
	}{
		{"GIB", 1 << 30}, {"GB", 1 << 30}, {"G", 1 << 30},
		{"MIB", 1 << 20}, {"MB", 1 << 20}, {"M", 1 << 20},
		{"KIB", 1 << 10}, {"KB", 1 << 10}, {"K", 1 << 10},
	} {
		if v, ok := strings.CutSuffix(upper, suf.suffix); ok {
			upper, mult = v, suf.mult
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(upper), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("config: %s must be a positive byte count like 16MiB, got %q", key, raw)
	}
	return n * mult, nil
}

func requiredURL(key string) (*url.URL, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return nil, fmt.Errorf("config: %s is required", key)
	}
	return parseURL(key, raw)
}

func parseURL(key, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil {
		return nil, fmt.Errorf("config: %s is not a valid URL: %w", key, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("config: %s must use http or https, got %q", key, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("config: %s has no host", key)
	}
	if u.Path != "" {
		// Serving from a sub-path is not supported, and accepting the value
		// while ignoring it would produce links that quietly point elsewhere.
		return nil, fmt.Errorf("config: %s must not include a path, got %q", key, u.Path)
	}
	return u, nil
}

func mustBeDirectory(key, dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Creating it here would turn a typo into a silently empty
			// instance, which looks like data loss to whoever deployed it.
			return fmt.Errorf("config: %s %q does not exist", key, dir)
		}
		return fmt.Errorf("config: %s %q: %w", key, dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("config: %s %q is not a directory", key, dir)
	}
	return nil
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
