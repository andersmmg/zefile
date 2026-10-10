package config

import (
	"strings"
	"testing"
	"time"
)

// setBaseEnv puts [Load] in a known state so a developer's shell cannot
// leak into a test.
func setBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvAppURL, "http://localhost:8080")
	t.Setenv(EnvConfigDir, t.TempDir())
	t.Setenv(EnvDataDir, "")
	t.Setenv(EnvContentURL, "")
	t.Setenv(EnvListen, "")
	t.Setenv(EnvReserve, "")
	t.Setenv(EnvReadOnly, "")
	t.Setenv(EnvStorageBackend, "")
	t.Setenv(EnvS3Endpoint, "")
	t.Setenv(EnvS3Region, "")
	t.Setenv(EnvS3Bucket, "")
	t.Setenv(EnvS3Prefix, "")
	t.Setenv(EnvS3AccessKeyID, "")
	t.Setenv(EnvS3SecretAccessKey, "")
	t.Setenv(EnvS3ForcePathStyle, "")
	t.Setenv(EnvPreviewMaxBytes, "")
	t.Setenv(EnvThumbMaxBytes, "")
}

func TestBackendDefaultsToLocal(t *testing.T) {
	setBaseEnv(t)
	t.Setenv(EnvDataDir, t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backend != BackendLocal {
		t.Errorf("Backend = %q, want %q", cfg.Backend, BackendLocal)
	}
}

func TestLocalBackendRequiresDataDir(t *testing.T) {
	setBaseEnv(t)

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), EnvDataDir) {
		t.Fatalf("Load without a data dir = %v, want an %s error", err, EnvDataDir)
	}

	// The data directory must exist, not merely be named.
	_, err = Load()
	if err == nil {
		t.Fatal("Load with an empty data dir succeeded")
	}
}

func TestUnknownBackendRefused(t *testing.T) {
	setBaseEnv(t)
	t.Setenv(EnvDataDir, t.TempDir())
	t.Setenv(EnvStorageBackend, "b2-only-ish")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), EnvStorageBackend) {
		t.Fatalf("Load with an unknown backend = %v, want an %s error", err, EnvStorageBackend)
	}
}

func TestS3BackendValidation(t *testing.T) {
	setBaseEnv(t)
	t.Setenv(EnvStorageBackend, BackendS3)

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvS3Bucket) {
		t.Fatalf("Load without a bucket = %v, want an %s error", err, EnvS3Bucket)
	}

	t.Setenv(EnvS3Bucket, "files")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvS3Region) {
		t.Fatalf("Load without a region = %v, want an %s error", err, EnvS3Region)
	}

	t.Setenv(EnvS3Region, "us-west-004")
	t.Setenv(EnvS3Endpoint, "s3.us-west-004.backblazeb2.com") // no scheme
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvS3Endpoint) {
		t.Fatalf("Load with a schemeless endpoint = %v, want an %s error", err, EnvS3Endpoint)
	}

	t.Setenv(EnvS3Endpoint, "https://s3.us-west-004.backblazeb2.com")
	t.Setenv(EnvS3Prefix, "/data/")
	t.Setenv(EnvS3ForcePathStyle, "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backend != BackendS3 {
		t.Errorf("Backend = %q, want %q", cfg.Backend, BackendS3)
	}
	if cfg.S3.Bucket != "files" || cfg.S3.Region != "us-west-004" {
		t.Errorf("S3 settings = %+v", cfg.S3)
	}
	if cfg.S3.Prefix != "data" {
		t.Errorf("Prefix = %q, want %q (slashes trimmed)", cfg.S3.Prefix, "data")
	}
	if !cfg.S3.ForcePathStyle {
		t.Error("ForcePathStyle = false, want true")
	}
	// No local tree in this mode: the data directory is cleared.
	if cfg.DataDir != "" {
		t.Errorf("DataDir = %q, want empty in s3 mode", cfg.DataDir)
	}
	// The base env must not leak the new optional knobs either: without
	// them the S3 settings resolve to the compiled defaults.
	if cfg.S3.PartSize != DefaultS3PartSize || cfg.S3.PresignTTL != DefaultS3PresignTTL {
		t.Errorf("S3 defaults = %+v, want part size %d and TTL %v", cfg.S3, DefaultS3PartSize, DefaultS3PresignTTL)
	}
}

func TestPreviewMaxBytes(t *testing.T) {
	setBaseEnv(t)
	t.Setenv(EnvDataDir, t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PreviewMaxBytes != DefaultPreviewMaxBytes {
		t.Errorf("PreviewMaxBytes = %d, want default %d", cfg.PreviewMaxBytes, DefaultPreviewMaxBytes)
	}

	t.Setenv(EnvPreviewMaxBytes, "512KiB")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PreviewMaxBytes != 512<<10 {
		t.Errorf("PreviewMaxBytes = %d, want %d", cfg.PreviewMaxBytes, 512<<10)
	}

	t.Setenv(EnvPreviewMaxBytes, "zero")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvPreviewMaxBytes) {
		t.Fatalf("Load with a bad size = %v, want an %s error", err, EnvPreviewMaxBytes)
	}
}

func TestThumbMaxBytes(t *testing.T) {
	setBaseEnv(t)
	t.Setenv(EnvDataDir, t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ThumbMaxBytes != DefaultThumbMaxBytes {
		t.Errorf("ThumbMaxBytes = %d, want default %d", cfg.ThumbMaxBytes, DefaultThumbMaxBytes)
	}

	t.Setenv(EnvThumbMaxBytes, "1GiB")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ThumbMaxBytes != 1<<30 {
		t.Errorf("ThumbMaxBytes = %d, want %d", cfg.ThumbMaxBytes, 1<<30)
	}
}

func TestS3PartSizeAndPresignTTL(t *testing.T) {
	setBaseEnv(t)
	t.Setenv(EnvStorageBackend, BackendS3)
	t.Setenv(EnvS3Bucket, "files")
	t.Setenv(EnvS3Region, "us-west-004")

	// Unset means compiled defaults, not zero: downstream code must never
	// see a part size the provider would refuse or a TTL that is instant.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.S3.PartSize != DefaultS3PartSize {
		t.Errorf("PartSize = %d, want default %d", cfg.S3.PartSize, DefaultS3PartSize)
	}
	if cfg.S3.PresignTTL != DefaultS3PresignTTL {
		t.Errorf("PresignTTL = %v, want default %v", cfg.S3.PresignTTL, DefaultS3PresignTTL)
	}

	for _, tc := range []struct {
		raw  string
		want int64
	}{
		{"16777216", 16 << 20},
		{"16MiB", 16 << 20},
		{"64MB", 64 << 20},
		{"1g", 1 << 30},
	} {
		t.Setenv(EnvS3PartSize, tc.raw)
		cfg, err := Load()
		if err != nil {
			t.Errorf("Load with %s=%q: %v", EnvS3PartSize, tc.raw, err)
			continue
		}
		if cfg.S3.PartSize != tc.want {
			t.Errorf("%s=%q: PartSize = %d, want %d", EnvS3PartSize, tc.raw, cfg.S3.PartSize, tc.want)
		}
	}

	for _, raw := range []string{"1MiB", "0", "-5MiB", "huge", "16XB"} {
		t.Setenv(EnvS3PartSize, raw)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvS3PartSize) {
			t.Errorf("Load with %s=%q = %v, want an %s error", EnvS3PartSize, raw, err, EnvS3PartSize)
		}
	}
	t.Setenv(EnvS3PartSize, "")

	for _, raw := range []string{"0", "-1m", "eternity"} {
		t.Setenv(EnvS3PresignTTL, raw)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvS3PresignTTL) {
			t.Errorf("Load with %s=%q = %v, want an %s error", EnvS3PresignTTL, raw, err, EnvS3PresignTTL)
		}
	}
	t.Setenv(EnvS3PresignTTL, "30m")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load with a TTL: %v", err)
	}
	if cfg.S3.PresignTTL != 30*time.Minute {
		t.Errorf("PresignTTL = %v, want 30m", cfg.S3.PresignTTL)
	}
}

func TestS3PublicEndpoint(t *testing.T) {
	setBaseEnv(t)
	t.Setenv(EnvStorageBackend, BackendS3)
	t.Setenv(EnvS3Bucket, "files")
	t.Setenv(EnvS3Region, "us-west-004")

	// Unset means the endpoint: one address serves all.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.S3.PublicEndpoint != "" {
		t.Errorf("PublicEndpoint = %q, want empty", cfg.S3.PublicEndpoint)
	}

	t.Setenv(EnvS3PublicEndpoint, "not-a-url")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvS3PublicEndpoint) {
		t.Fatalf("Load with a bad public endpoint = %v, want an %s error", err, EnvS3PublicEndpoint)
	}

	t.Setenv(EnvS3PublicEndpoint, "http://localhost:8334")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.S3.PublicEndpoint != "http://localhost:8334" {
		t.Errorf("PublicEndpoint = %q", cfg.S3.PublicEndpoint)
	}
}

func TestS3CredentialsMustPair(t *testing.T) {
	setBaseEnv(t)
	t.Setenv(EnvStorageBackend, BackendS3)
	t.Setenv(EnvS3Bucket, "files")
	t.Setenv(EnvS3Region, "us-west-004")
	t.Setenv(EnvS3AccessKeyID, "keyid")
	t.Setenv(EnvS3SecretAccessKey, "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), EnvS3SecretAccessKey) {
		t.Fatalf("Load with a lone access key = %v, want an %s error", err, EnvS3SecretAccessKey)
	}

	// A full pair is accepted, and an empty AccessKeyID is accepted too —
	// that is the documented "fall through to the ambient AWS chain" mode.
	t.Setenv(EnvS3SecretAccessKey, "secret")
	if _, err := Load(); err != nil {
		t.Fatalf("Load with a credential pair: %v", err)
	}
	t.Setenv(EnvS3AccessKeyID, "")
	t.Setenv(EnvS3SecretAccessKey, "orphan-secret")
	if _, err := Load(); err != nil {
		t.Fatalf("Load with a secret but no key id: %v", err)
	}
}
