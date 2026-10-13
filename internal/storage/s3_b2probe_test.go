package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// B2 probe: verifies provider behavior against a real Backblaze B2 bucket
// (presigned-URL overrides, conditional writes, checksums, stored metadata,
// delimiter listing, SDK compatibility). Skipped unless the B2 variables are
// set, so the ordinary suite never needs credentials:
//
//	ZEFILE_TEST_B2_ENDPOINT=https://s3.us-west-004.backblazeb2.com \
//	ZEFILE_TEST_B2_BUCKET=<bucket> \
//	ZEFILE_TEST_B2_KEY_ID=<application-key-id> \
//	ZEFILE_TEST_B2_KEY=<application-key> \
//	go test ./internal/storage -run TestS3B2Probe -v
//
// ZEFILE_TEST_B2_REGION overrides the signing region, which otherwise comes
// from the endpoint hostname (s3.<region>.backblazeb2.com).
//
// Everything lives under one unique prefix per run and is deleted
// afterwards. Each subtest reports its answer and keeps going.

type b2probe struct {
	t        *testing.T
	backend  *S3
	bucket   string
	endpoint string
	prefix   string
}

func newB2Probe(t *testing.T) *b2probe {
	t.Helper()
	endpoint := os.Getenv("ZEFILE_TEST_B2_ENDPOINT")
	bucket := os.Getenv("ZEFILE_TEST_B2_BUCKET")
	keyID := os.Getenv("ZEFILE_TEST_B2_KEY_ID")
	secret := os.Getenv("ZEFILE_TEST_B2_KEY")
	if endpoint == "" || bucket == "" || keyID == "" || secret == "" {
		t.Skip("ZEFILE_TEST_B2_ENDPOINT/BUCKET/KEY_ID/KEY are not all set; skipping the B2 probe")
	}
	region := os.Getenv("ZEFILE_TEST_B2_REGION")
	if region == "" {
		// s3.<region>.backblazeb2.com — the region signs every request.
		rest, ok := strings.CutPrefix(strings.TrimPrefix(endpoint, "https://"), "s3.")
		if host, _, _ := strings.Cut(rest, "/"); ok {
			region, _, _ = strings.Cut(host, ".")
		}
	}
	if region == "" {
		t.Fatalf("cannot derive the region from %q; set ZEFILE_TEST_B2_REGION", endpoint)
	}
	cfg := S3Config{
		Endpoint: endpoint, Region: region, Bucket: bucket,
		AccessKeyID: keyID, SecretAccessKey: secret,
		ForcePathStyle: true,
		Prefix:         "zefile-b2probe",
	}
	s, err := OpenS3(cfg)
	if err != nil {
		t.Fatalf("OpenS3 against B2: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	p := &b2probe{t: t, backend: s, bucket: bucket, endpoint: endpoint}
	t.Cleanup(func() { p.cleanup() })
	return p
}

func (p *b2probe) ctx() context.Context { return context.Background() }

// key seeds an object under the run prefix and reports its key.
func (p *b2probe) key(t *testing.T, name string, body []byte) string {
	t.Helper()
	k := "zefile-b2probe/" + name
	_, err := p.backend.client.PutObject(p.ctx(), &awss3.PutObjectInput{
		Bucket: aws.String(p.bucket), Key: aws.String(k), Body: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	return k
}

func (p *b2probe) cleanup() {
	ctx := context.Background()
	var token *string
	for {
		out, err := p.backend.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket: aws.String(p.bucket), Prefix: aws.String("zefile-b2probe/"),
			ContinuationToken: token,
		})
		if err != nil {
			p.t.Logf("probe cleanup list: %v", err)
			return
		}
		for _, obj := range out.Contents {
			_, _ = p.backend.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
				Bucket: aws.String(p.bucket), Key: obj.Key,
			})
		}

		uploads, err := p.backend.client.ListMultipartUploads(ctx, &awss3.ListMultipartUploadsInput{
			Bucket: aws.String(p.bucket), Prefix: aws.String("zefile-b2probe/"),
		})
		if err == nil {
			for _, up := range uploads.Uploads {
				_, _ = p.backend.client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
					Bucket: aws.String(p.bucket), Key: up.Key, UploadId: up.UploadId,
				})
			}
		}
		if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			return
		}
		token = out.NextContinuationToken
	}
}

// Item 6 first: if the SDK cannot talk to B2 at all, nothing below matters.
func TestS3B2ProbeSmoke(t *testing.T) {
	p := newB2Probe(t)
	ctx := p.ctx()
	p.key(t, "smoke/hello.txt", []byte("hello b2"))

	info, err := p.backend.Stat(ctx, MustParsePath("/smoke/hello.txt"))
	if err != nil {
		t.Fatalf("§9.6 Stat through the backend: %v", err)
	}
	if info.Size != 8 {
		t.Errorf("§9.6 Stat size = %d, want 8", info.Size)
	}
	entries, err := p.backend.List(ctx, MustParsePath("/smoke"))
	if err != nil || len(entries) != 1 {
		t.Errorf("§9.6 List = %+v, %v; want one entry", entries, err)
	}
	t.Logf("§9.6 SDK + path-style + Stat/List against %s: OK", p.endpoint)
}

// Item 1: per-request overrides on a presigned GET.
func TestS3B2ProbeDispositionOverride(t *testing.T) {
	p := newB2Probe(t)
	ctx := p.ctx()
	p.key(t, "disp/file.bin", []byte("disposition probe"))

	dl, err := p.backend.PresignGet(ctx, MustParsePath("/disp/file.bin"), false, 0,
		"inline; filename*=UTF-8''probe.bin")
	if err != nil {
		t.Fatalf("§9.1 PresignGet: %v", err)
	}
	resp, err := http.Get(dl)
	if err != nil {
		t.Fatalf("§9.1 GET presigned URL: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "inline") {
		t.Errorf("§9.1 Content-Disposition = %q, want the inline override (B2 ignores response-* overrides)", got)
	} else {
		t.Logf("§9.1 B2 honours response-content-disposition: OK")
	}

	// Same question for the content type.
	typeKey := "zefile-b2probe/disp/typed.bin"
	_, err = p.backend.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(p.bucket), Key: aws.String(typeKey),
		Body: bytes.NewReader([]byte("x")), ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		t.Fatalf("§9.1 seed typed object: %v", err)
	}
	typedReq, err := p.backend.signer().PresignGetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(p.bucket), Key: aws.String(typeKey),
		ResponseContentType: aws.String("image/png"),
	}, awss3.WithPresignExpires(time.Minute))
	if err != nil {
		t.Fatalf("§9.1 presign with type override: %v", err)
	}
	tresp, err := http.Get(typedReq.URL)
	if err != nil {
		t.Fatalf("§9.1 GET typed URL: %v", err)
	}
	defer tresp.Body.Close()
	io.Copy(io.Discard, tresp.Body)
	if got := tresp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("§9.1 Content-Type = %q, want the image/png override", got)
	} else {
		t.Logf("§9.1 B2 honours response-content-type: OK")
	}
}

// Item 4: metadata stored at write time comes back on HEAD.
func TestS3B2ProbeStoredMetadata(t *testing.T) {
	p := newB2Probe(t)
	ctx := p.ctx()
	k := "zefile-b2probe/meta/file.bin"
	_, err := p.backend.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(p.bucket), Key: aws.String(k), Body: bytes.NewReader([]byte("meta")),
		ContentType: aws.String("image/png"), ContentDisposition: aws.String("attachment; filename=\"f.png\""),
	})
	if err != nil {
		t.Fatalf("§9.4 PutObject with metadata: %v", err)
	}
	out, err := p.backend.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(p.bucket), Key: aws.String(k),
	})
	if err != nil {
		t.Fatalf("§9.4 HeadObject: %v", err)
	}
	if got := aws.ToString(out.ContentType); got != "image/png" {
		t.Errorf("§9.4 Content-Type = %q, want image/png", got)
	} else {
		t.Logf("§9.4 stored Content-Type round-trips: OK")
	}
	if got := aws.ToString(out.ContentDisposition); got != "attachment; filename=\"f.png\"" {
		t.Errorf("§9.4 Content-Disposition = %q, want the stored attachment value", got)
	} else {
		t.Logf("§9.4 stored Content-Disposition round-trips: OK")
	}
}

// Item 2: conditional writes.
func TestS3B2ProbeConditionalWrite(t *testing.T) {
	p := newB2Probe(t)
	ctx := p.ctx()
	k := "zefile-b2probe/cond/file.bin"

	_, err := p.backend.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(p.bucket), Key: aws.String(k),
		Body: bytes.NewReader([]byte("first")), IfNoneMatch: aws.String("*"),
	})
	if err != nil {

		t.Logf("§9.2 conditional PutObject: refused (%v) — no conditional writes on B2", err)
		return
	}
	t.Logf("§9.2 conditional PutObject on a new key: accepted")

	_, err = p.backend.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(p.bucket), Key: aws.String(k),
		Body: bytes.NewReader([]byte("second")), IfNoneMatch: aws.String("*"),
	})
	if err == nil {

		_, _ = p.backend.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
			Bucket: aws.String(p.bucket), Key: aws.String(k),
		})
		t.Errorf("§9.2 conditional PutObject on an existing key SUCCEEDED — the precondition is ignored")
	} else {
		t.Logf("§9.2 conditional PutObject on an existing key refused (%v): OK", err)
	}
}

// Item 3: per-part checksums on UploadPart.
func TestS3B2ProbePartChecksum(t *testing.T) {
	p := newB2Probe(t)
	ctx := p.ctx()
	k := "zefile-b2probe/sum/parts.bin"

	up, err := p.backend.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket: aws.String(p.bucket), Key: aws.String(k),
	})
	if err != nil {
		t.Fatalf("§9.3 CreateMultipartUpload: %v", err)
	}
	uploadID := aws.ToString(up.UploadId)
	t.Cleanup(func() {
		_, _ = p.backend.client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
			Bucket: aws.String(p.bucket), Key: aws.String(k), UploadId: aws.String(uploadID),
		})
	})

	body := bytes.Repeat([]byte("checksum "), 1<<12)
	sum := sha256.Sum256(body)
	part, err := p.backend.client.UploadPart(ctx, &awss3.UploadPartInput{
		Bucket: aws.String(p.bucket), Key: aws.String(k), UploadId: aws.String(uploadID),
		PartNumber: aws.Int32(1), Body: bytes.NewReader(body),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(sum[:])),
	})
	if err != nil {
		t.Errorf("§9.3 UploadPart with x-amz-checksum-sha256: %v (B2 does not take per-part SHA-256)", err)
		return
	}
	t.Logf("§9.3 UploadPart with x-amz-checksum-sha256 accepted (ETag %s): OK", aws.ToString(part.ETag))
}

// Item 5: delimiter listings, markers, and ListParts through the backend.
func TestS3B2ProbeListing(t *testing.T) {
	p := newB2Probe(t)
	ctx := p.ctx()
	s := p.backend

	if err := s.MkdirAll(ctx, MustParsePath("/ls/empty")); err != nil {
		t.Fatalf("§9.5 MkdirAll: %v", err)
	}
	p.key(t, "ls/empty/nested.txt", []byte("nested"))

	// The marker-only directory lists as a directory with no children.
	entries, err := s.List(ctx, MustParsePath("/ls"))
	if err != nil {
		t.Fatalf("§9.5 List: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = e.IsDir
	}
	if !names["empty"] {
		t.Errorf("§9.5 List(/ls) = %+v; want the empty directory", entries)
	} else {
		t.Logf("§9.5 delimiter grouping with a marker-only directory: OK")
	}

	// ListParts through the backend: one part PUT, then the resume offset.
	target := MustParsePath("/ls/resume.bin")
	body := bytes.Repeat([]byte("resume "), 1<<12) // 48 KiB
	handle, err := s.BeginUpload(ctx, target, int64(len(body)))
	if err != nil {
		t.Fatalf("§9.5 BeginUpload: %v", err)
	}
	t.Cleanup(func() { _ = s.AbortUpload(ctx, handle) })
	urls, err := s.PresignParts(ctx, handle, []int{1})
	if err != nil || len(urls) != 1 {
		t.Fatalf("§9.5 PresignParts: %+v, %v", urls, err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, urls[0].URL, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("§9.5 PUT part: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("§9.5 PUT part status: %d", resp.StatusCode)
	}
	if got, err := s.CommittedBytes(ctx, handle); err != nil || got != int64(len(body)) {
		t.Errorf("§9.5 CommittedBytes = %d, %v; want %d (ListParts behaviour)", got, err, len(body))
	} else {
		t.Logf("§9.5 ListParts-backed resume offset: OK")
	}
	if err := s.CompleteUpload(ctx, handle, target); err != nil {
		t.Errorf("§9.5 CompleteUpload: %v", err)
	}
}

// Marker as a CopyObject source, and UploadPartCopy mechanics.
func TestS3B2ProbeServerSideCopy(t *testing.T) {
	p := newB2Probe(t)
	ctx := p.ctx()
	s := p.backend

	if err := s.Mkdir(ctx, MustParsePath("/cp")); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	marker := "zefile-b2probe/cp/"
	_, err := s.client.CopyObject(ctx, &awss3.CopyObjectInput{
		Bucket: aws.String(p.bucket), Key: aws.String("zefile-b2probe/cp-copy/"),
		CopySource: aws.String(s.copySource(marker)),
	})
	if err != nil {
		t.Logf("trailing-slash key as CopyObject source: refused (%v) — marker recreation stays", err)
	} else {
		t.Logf("trailing-slash key as CopyObject source: ACCEPTED (unlike SeaweedFS)")
	}

	src := p.key(t, "cp/big.bin", bytes.Repeat([]byte("partcopy "), 1<<12))
	dst := "zefile-b2probe/cp/big-copy.bin"
	up, err := s.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket: aws.String(p.bucket), Key: aws.String(dst),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload for part-copy: %v", err)
	}
	uploadID := aws.ToString(up.UploadId)
	part, err := s.client.UploadPartCopy(ctx, &awss3.UploadPartCopyInput{
		Bucket: aws.String(p.bucket), Key: aws.String(dst), UploadId: aws.String(uploadID),
		PartNumber: aws.Int32(1), CopySource: aws.String(s.copySource(src)),
	})
	if err != nil {
		t.Errorf("UploadPartCopy: %v (multipart copies past 4 GiB cannot stay server-side)", err)
		_, _ = s.client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
			Bucket: aws.String(p.bucket), Key: aws.String(dst), UploadId: aws.String(uploadID),
		})
		return
	}
	_, err = s.client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket: aws.String(p.bucket), Key: aws.String(dst), UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{ETag: part.CopyPartResult.ETag, PartNumber: aws.Int32(1)},
		}},
	})
	if err != nil {
		t.Errorf("CompleteMultipartUpload of a copied part: %v", err)
		return
	}
	t.Logf("UploadPartCopy + complete: OK")
}

func TestS3B2ProbeObjectChecksum(t *testing.T) {
	p := newB2Probe(t)
	ctx := p.ctx()
	s := p.backend
	body := []byte("zefile whole-object checksum probe")
	sum := sha256.Sum256(body)
	wantB64 := base64.StdEncoding.EncodeToString(sum[:])
	wantHex := hex.EncodeToString(sum[:])

	// §9.6a: PutObject with the algorithm — accepted, stored, returned.
	k := "zefile-b2probe/sum/put.bin"
	_, err := s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(p.bucket), Key: aws.String(k),
		Body: bytes.NewReader(body), ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
	})
	if err != nil {
		t.Errorf("§9.6 PutObject with ChecksumAlgorithm=SHA256: %v", err)
	} else {
		out, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{
			Bucket: aws.String(p.bucket), Key: aws.String(k), ChecksumMode: types.ChecksumModeEnabled,
		})
		if err != nil {
			t.Errorf("§9.6 HeadObject with ChecksumMode: %v", err)
		} else if got := aws.ToString(out.ChecksumSHA256); got != wantB64 {
			t.Errorf("§9.6 stored checksum = %q, want %q", got, wantB64)
		} else {
			t.Logf("§9.6 PutObject checksum stored and returned: OK")
		}
	}
	k2 := p.key(t, "sum/copied.bin", body)
	_, err = s.client.CopyObject(ctx, &awss3.CopyObjectInput{
		Bucket: aws.String(p.bucket), Key: aws.String(k2),
		CopySource:        aws.String(s.copySource(k2)),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
	})
	if err != nil {
		t.Logf("§9.6 provider refused the checksummed copy outright: %v", err)
	} else {
		out, herr := s.client.HeadObject(ctx, &awss3.HeadObjectInput{
			Bucket: aws.String(p.bucket), Key: aws.String(k2), ChecksumMode: types.ChecksumModeEnabled,
		})
		if herr != nil {
			t.Logf("§9.6 HeadObject after the copy: %v", herr)
		} else if got := aws.ToString(out.ChecksumSHA256); got == "" {
			t.Logf("§9.6 copy accepted but nothing stored — the known B2 no-op")
		} else if got != wantB64 {
			t.Logf("§9.6 checksum after copy = %q (whole object would be %q)", got, wantB64)
		} else {
			t.Logf("§9.6 self-copy with algorithm computed the checksum: OK")
		}
	}

	// multipart upload path stores no checksum on any provider
	if err := s.MkdirAll(ctx, MustParsePath("/sum")); err != nil {
		t.Fatalf("§9.6 MkdirAll: %v", err)
	}
	target := MustParsePath("/sum/flow.bin")
	handle, err := s.BeginUpload(ctx, target, int64(len(body)))
	if err != nil {
		t.Fatalf("§9.6 BeginUpload: %v", err)
	}
	t.Cleanup(func() { _ = s.AbortUpload(ctx, handle) })
	urls, err := s.PresignParts(ctx, handle, []int{1})
	if err != nil || len(urls) != 1 {
		t.Fatalf("§9.6 PresignParts: %+v, %v", urls, err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, urls[0].URL, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("§9.6 PUT part: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("§9.6 PUT part status: %d", resp.StatusCode)
	}
	if err := s.CompleteUpload(ctx, handle, target); err != nil {
		t.Fatalf("§9.6 CompleteUpload: %v", err)
	}
	if got, err := s.ObjectChecksum(ctx, target); err == nil {
		t.Errorf("§9.6 ObjectChecksum after a multipart upload = %q, want a refusal: nothing can store a whole-object SHA-256 across parts", got)
	} else {
		t.Logf("§9.6 multipart upload stores no checksum (the file's SHA-256 is %q), refused as designed: %v", wantHex, err)
	}
}

func TestS3B2ProbeMultipartChecksum(t *testing.T) {
	p := newB2Probe(t)
	ctx := p.ctx()
	s := p.backend
	body := []byte("zefile multipart checksum probe")
	sum := sha256.Sum256(body)
	wantB64 := base64.StdEncoding.EncodeToString(sum[:])

	probe := func(name string, createChecksum, partChecksum bool, typ types.ChecksumType) {
		k := "zefile-b2probe/" + name + ".bin"
		in := &awss3.CreateMultipartUploadInput{
			Bucket: aws.String(p.bucket), Key: aws.String(k),
		}
		if createChecksum {
			in.ChecksumAlgorithm = types.ChecksumAlgorithmSha256
			in.ChecksumType = typ
		}
		create, err := s.client.CreateMultipartUpload(ctx, in)
		if err != nil {
			if strings.Contains(err.Error(), "InvalidRequest") {
				t.Logf("§9.7 %s: create refused (semantics): %v", name, err)
				return
			}
			t.Errorf("§9.7 %s: create multipart: %v", name, err)
			return
		}
		part := &awss3.UploadPartInput{
			Bucket: aws.String(p.bucket), Key: aws.String(k),
			UploadId: create.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(body),
		}
		if partChecksum {
			part.ChecksumAlgorithm = types.ChecksumAlgorithmSha256
		}
		up, err := s.client.UploadPart(ctx, part)
		if err != nil {
			_, _ = s.client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
				Bucket: aws.String(p.bucket), Key: aws.String(k), UploadId: create.UploadId,
			})
			if strings.Contains(err.Error(), "InvalidRequest") {
				t.Logf("§9.7 %s: part refused (semantics): %v", name, err)
				return
			}
			t.Errorf("§9.7 %s: upload part: %v", name, err)
			return
		}
		_, err = s.client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
			Bucket: aws.String(p.bucket), Key: aws.String(k), UploadId: create.UploadId,
			MultipartUpload: &types.CompletedMultipartUpload{
				Parts: []types.CompletedPart{{ETag: up.ETag, PartNumber: aws.Int32(1)}},
			},
		})
		if err != nil {
			if strings.Contains(err.Error(), "InvalidRequest") {
				t.Logf("§9.7 %s: complete refused (semantics): %v", name, err)
				return
			}
			t.Errorf("§9.7 %s: complete multipart: %v", name, err)
			return
		}
		out, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{
			Bucket: aws.String(p.bucket), Key: aws.String(k), ChecksumMode: types.ChecksumModeEnabled,
		})
		if err != nil {
			t.Errorf("§9.7 %s: head: %v", name, err)
			return
		}
		t.Logf("§9.7 %s: complete OK — type=%q sha256=%q (whole-object would be %q)",
			name, string(out.ChecksumType), aws.ToString(out.ChecksumSHA256), wantB64)
	}

	probe("plain-part-under-sha256-create", true, false, "")
	probe("sdk-checksummed-part", true, true, "")
	// AWS reserves FULL_OBJECT for the CRC family; B2 might not
	probe("full-object-type", true, false, types.ChecksumTypeFullObject)
}
