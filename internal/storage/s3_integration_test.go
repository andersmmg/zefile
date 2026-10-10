package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// Integration tests against a real S3-compatible server. They skip unless
// ZEFILE_TEST_S3_ENDPOINT is set, so the ordinary suite stays self-contained
// (PLAN.md §8). Locally the server is SeaweedFS:
//
//	weed mini -dir=./weed_temp    (SeaweedFS quick-start; S3 on :8333,
//	identity admin/secret, bucket my-bucket)
//	ZEFILE_TEST_S3_ENDPOINT=http://127.0.0.1:8333 \
//	ZEFILE_TEST_S3_ACCESS_KEY_ID=admin ZEFILE_TEST_S3_SECRET_ACCESS_KEY=secret \
//	ZEFILE_TEST_S3_BUCKET=my-bucket \
//	go test ./internal/storage -run Integration -v
//
// Against Backblaze B2 the same variables point at
// https://s3.<region>.backblazeb2.com with an application key and a real
// bucket name.

func integrationConfig(t *testing.T) S3Config {
	t.Helper()
	endpoint := os.Getenv("ZEFILE_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("ZEFILE_TEST_S3_ENDPOINT is not set; skipping the real-server S3 integration test")
	}
	bucket := envOr("ZEFILE_TEST_S3_BUCKET", "zefile-test")
	cfg := S3Config{
		Endpoint:        endpoint,
		Region:          envOr("ZEFILE_TEST_S3_REGION", "us-east-1"),
		Bucket:          bucket,
		AccessKeyID:     os.Getenv("ZEFILE_TEST_S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("ZEFILE_TEST_S3_SECRET_ACCESS_KEY"),
		ForcePathStyle:  envOr("ZEFILE_TEST_S3_PATH_STYLE", "true") == "true",
	}
	return cfg
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func TestS3IntegrationRoundTrip(t *testing.T) {
	cfg := integrationConfig(t)
	ctx := context.Background()

	// Probe with the bare config first: OpenS3 needs the bucket to exist.
	probe, err := OpenS3(cfg)
	if err != nil {
		// One attempt to create it — SeaweedFS and MinIO start empty; on B2
		// the bucket must pre-exist and this simply reports why not.
		client, cerr := newS3Client(cfg, cfg.Endpoint)
		if cerr != nil {
			t.Fatalf("OpenS3: %v (building a client to create the bucket: %v)", err, cerr)
		}
		if _, cerr := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(cfg.Bucket)}); cerr != nil {
			t.Fatalf("OpenS3: %v (and CreateBucket: %v)", err, cerr)
		}
		probe, err = OpenS3(cfg)
		if err != nil {
			t.Fatalf("OpenS3 after CreateBucket: %v", err)
		}
	}

	// A unique prefix per run keeps repeated runs independent.
	cfg.Prefix = fmt.Sprintf("zefile-it-%d", time.Now().UnixNano())
	s, err := OpenS3(cfg)
	if err != nil {
		t.Fatalf("OpenS3: %v", err)
	}
	t.Cleanup(func() { cleanupPrefix(t, probe, cfg.Prefix) })

	// Directory creation: markers survive round trips.
	if err := s.MkdirAll(ctx, MustParsePath("/alpha/beta/gamma")); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dir, err := s.Stat(ctx, MustParsePath("/alpha/beta"))
	if err != nil || !dir.IsDir {
		t.Fatalf("Stat(/alpha/beta) = %+v, %v; want a directory", dir, err)
	}

	// A file placed directly into the bucket — the externally-created case.
	body := bytes.Repeat([]byte("zefile "), 1<<16) // 448 KiB
	key := cfg.Prefix + "/alpha/beta/gamma/file.bin"
	if _, err := probe.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(cfg.Bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	}); err != nil {
		t.Fatalf("seed PutObject: %v", err)
	}

	file, err := s.Stat(ctx, MustParsePath("/alpha/beta/gamma/file.bin"))
	if err != nil {
		t.Fatalf("Stat of the seeded file: %v", err)
	}
	if file.Size != int64(len(body)) {
		t.Errorf("Stat size = %d, want %d", file.Size, len(body))
	}

	entries, err := s.List(ctx, MustParsePath("/alpha/beta/gamma"))
	if err != nil || len(entries) != 1 || entries[0].Name != "file.bin" {
		t.Fatalf("List(/alpha/beta/gamma) = %+v, %v; want file.bin", entries, err)
	}

	root, err := s.List(ctx, MustParsePath("/"))
	if err != nil || len(root) != 1 || root[0].Name != "alpha" || !root[0].IsDir {
		t.Fatalf("List(/) = %+v, %v; want only the alpha directory", root, err)
	}

	// Server-side reads work on a real server.
	f, err := s.Open(ctx, MustParsePath("/alpha/beta/gamma/file.bin"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("Open read %d bytes, want %d identical", len(got), len(body))
	}
}

// TestS3IntegrationDirectUpload runs the whole write path against the real
// server: the browser flow without a browser.
func TestS3IntegrationDirectUpload(t *testing.T) {
	cfg := integrationConfig(t)
	ctx := context.Background()

	probe, err := OpenS3(cfg)
	if err != nil {
		t.Skipf("bucket %q is not reachable (create it first): %v", cfg.Bucket, err)
	}
	cfg.Prefix = fmt.Sprintf("zefile-it-up-%d", time.Now().UnixNano())
	s, err := OpenS3(cfg)
	if err != nil {
		t.Fatalf("OpenS3: %v", err)
	}
	t.Cleanup(func() { cleanupPrefix(t, probe, cfg.Prefix) })

	if err := s.MkdirAll(ctx, MustParsePath("/up")); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	body := bytes.Repeat([]byte("seaweed "), 1<<15) // 256 KiB, one part
	target := MustParsePath("/up/file.bin")
	handle, err := s.BeginUpload(ctx, target, int64(len(body)))
	if err != nil {
		t.Fatalf("BeginUpload: %v", err)
	}
	partURLs, err := s.PresignParts(ctx, handle, []int{1})
	if err != nil {
		t.Fatalf("PresignParts: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, partURLs[0].URL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build part PUT: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT part: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("PUT part: status %d", resp.StatusCode)
	}

	if got, err := s.CommittedBytes(ctx, handle); err != nil || got != int64(len(body)) {
		t.Fatalf("CommittedBytes = %d, %v; want %d", got, err, len(body))
	}
	if err := s.CompleteUpload(ctx, handle, target); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}

	// The download redirect target serves the bytes with the override.
	dl, err := s.PresignGet(ctx, target, false, 0, "inline; filename*=UTF-8''file.bin")
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	if !strings.HasPrefix(dl, cfg.Endpoint) {
		t.Errorf("download URL = %q, want it under the endpoint %q", dl, cfg.Endpoint)
	}
	got, err := http.Get(dl)
	if err != nil {
		t.Fatalf("GET presigned URL: %v", err)
	}
	fetched, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if !bytes.Equal(fetched, body) {
		t.Fatal("downloaded bytes differ from what was uploaded")
	}
	if disp := got.Header.Get("Content-Disposition"); !strings.HasPrefix(disp, "inline") {
		t.Errorf("Content-Disposition = %q, want the inline override", disp)
	}

	// Trash, restore and purge move the object server-side.
	id, err := s.Trash(ctx, target)
	if err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if err := s.Restore(ctx, id, target); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := s.Stat(ctx, target); err != nil {
		t.Fatalf("Stat after restore: %v", err)
	}
	id, err = s.Trash(ctx, target)
	if err != nil {
		t.Fatalf("Trash again: %v", err)
	}
	if err := s.PurgeTrash(ctx, id); err != nil {
		t.Fatalf("PurgeTrash: %v", err)
	}
	if _, err := s.Stat(ctx, target); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Stat after purge = %v, want ErrNotExist", err)
	}
}

// TestS3IntegrationCopyMove runs copy/move/remove against the real server,
// with byte verification through presigned downloads.
func TestS3IntegrationCopyMove(t *testing.T) {
	cfg := integrationConfig(t)
	ctx := context.Background()

	probe, err := OpenS3(cfg)
	if err != nil {
		t.Skipf("bucket %q is not reachable (create it first): %v", cfg.Bucket, err)
	}
	cfg.Prefix = fmt.Sprintf("zefile-it-cp-%d", time.Now().UnixNano())
	s, err := OpenS3(cfg)
	if err != nil {
		t.Fatalf("OpenS3: %v", err)
	}
	t.Cleanup(func() { cleanupPrefix(t, probe, cfg.Prefix) })

	if err := s.MkdirAll(ctx, MustParsePath("/docs/sub")); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := s.Mkdir(ctx, MustParsePath("/dst")); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	body := bytes.Repeat([]byte("copy me "), 1<<14) // 128 KiB
	for _, key := range []string{"/docs/leaf.bin", "/docs/sub/deep.bin"} {
		if _, err := probe.client.PutObject(ctx, &awss3.PutObjectInput{
			Bucket: aws.String(cfg.Bucket),
			Key:    aws.String(cfg.Prefix + key),
			Body:   bytes.NewReader(body),
		}); err != nil {
			t.Fatalf("seed PutObject %s: %v", key, err)
		}
	}
	fetch := func(p Path) []byte {
		t.Helper()
		dl, err := s.PresignGet(ctx, p, false, 0, "")
		if err != nil {
			t.Fatalf("PresignGet(%s): %v", p, err)
		}
		resp, err := http.Get(dl)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		defer resp.Body.Close()
		got, _ := io.ReadAll(resp.Body)
		return got
	}

	// Single-file copy: bytes identical, source untouched.
	if err := s.Copy(ctx, MustParsePath("/docs/leaf.bin"), MustParsePath("/dst/leaf-copy.bin")); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got := fetch(MustParsePath("/dst/leaf-copy.bin")); !bytes.Equal(got, body) {
		t.Fatal("copied bytes differ from the source")
	}
	if err := s.Copy(ctx, MustParsePath("/docs/leaf.bin"), MustParsePath("/dst/leaf-copy.bin")); !errors.Is(err, ErrExist) {
		t.Fatalf("Copy onto existing = %v, want ErrExist", err)
	}

	// Tree copy with progress, then a move of the whole tree.
	var done, total int64
	if err := s.CopyTree(ctx, MustParsePath("/docs"), MustParsePath("/dst/docs-tree"), func(d, tot int64) {
		done, total = d, tot
	}); err != nil {
		t.Fatalf("CopyTree: %v", err)
	}
	if done != total || total != 2*int64(len(body)) {
		t.Fatalf("CopyTree progress ended at %d/%d, want %d", done, total, 2*len(body))
	}
	if err := s.Move(ctx, MustParsePath("/dst/docs-tree"), MustParsePath("/docs-moved")); err != nil {
		t.Fatalf("Move dir: %v", err)
	}
	if got := fetch(MustParsePath("/docs-moved/sub/deep.bin")); !bytes.Equal(got, body) {
		t.Fatal("moved tree's bytes differ from the source")
	}
	if _, err := s.Stat(ctx, MustParsePath("/dst/docs-tree")); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Stat of the moved-away tree = %v, want ErrNotExist", err)
	}

	// Trashing and restoring a directory exercises the marker paths.
	tid, err := s.Trash(ctx, MustParsePath("/docs-moved"))
	if err != nil {
		t.Fatalf("Trash dir: %v", err)
	}
	if _, err := s.Stat(ctx, MustParsePath("/docs-moved")); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Stat of a trashed directory = %v, want ErrNotExist", err)
	}
	if err := s.Restore(ctx, tid, MustParsePath("/docs-moved")); err != nil {
		t.Fatalf("Restore dir: %v", err)
	}
	if got := fetch(MustParsePath("/docs-moved/sub/deep.bin")); !bytes.Equal(got, body) {
		t.Fatal("restored tree's bytes differ from the source")
	}

	// File move, then both deletes.
	if err := s.Move(ctx, MustParsePath("/dst/leaf-copy.bin"), MustParsePath("/renamed.bin")); err != nil {
		t.Fatalf("Move file: %v", err)
	}
	if got := fetch(MustParsePath("/renamed.bin")); !bytes.Equal(got, body) {
		t.Fatal("moved bytes differ from the source")
	}
	if err := s.Remove(ctx, MustParsePath("/docs")); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("Remove of a non-empty directory = %v, want ErrNotEmpty", err)
	}
	if err := s.Remove(ctx, MustParsePath("/renamed.bin")); err != nil {
		t.Fatalf("Remove file: %v", err)
	}
	if err := s.RemoveAll(ctx, MustParsePath("/docs")); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := s.Stat(ctx, MustParsePath("/docs")); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Stat after RemoveAll = %v, want ErrNotExist", err)
	}
}

// cleanupPrefix deletes everything under the run's prefix, best-effort.
func cleanupPrefix(t *testing.T, s *S3, prefix string) {
	t.Helper()
	ctx := context.Background()
	token := (*string)(nil)
	deleted := 0
	for {
		out, err := s.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix + "/"),
			ContinuationToken: token,
		})
		if err != nil {
			t.Logf("cleanup list: %v", err)
			return
		}
		for _, obj := range out.Contents {
			if _, err := s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
				Bucket: aws.String(s.bucket),
				Key:    obj.Key,
			}); err != nil {
				t.Logf("cleanup delete %s: %v", aws.ToString(obj.Key), err)
			}
			deleted++
		}
		if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			break
		}
		token = out.NextContinuationToken
	}
	if deleted > 0 {
		t.Logf("cleaned %d objects under %s/", deleted, prefix)
	}
}
