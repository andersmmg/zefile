package storage

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The stub speaks just enough of the S3 wire protocol for the read path
// (HeadBucket, HeadObject, ListObjectsV2, PutObject) and the write path
// (multipart create/part/list/complete/abort, single and batch delete,
// server-side copy, object GET with a body). It deliberately ignores
// signatures — SigV4 is the provider's business, and the seaweedfs-backed
// integration test (below, env-gated) exercises a real server end to end.

var stubModTime = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type stubObject struct {
	size    int64
	modTime time.Time
	body    []byte
}

// stubUpload is one in-progress multipart upload: the destination key and
// the parts received so far, by 1-based number.
type stubUpload struct {
	key   string
	parts map[int][]byte
}

type stubS3 struct {
	mu         sync.Mutex
	objects    map[string]stubObject
	uploads    map[string]*stubUpload
	nextUpload int
	pageSize   int // forces small pages to exercise pagination; 0 = unlimited
}

func newStubS3(seed ...string) *stubS3 {
	s := &stubS3{objects: make(map[string]stubObject), uploads: make(map[string]*stubUpload)}
	for _, key := range seed {
		s.objects[key] = stubObject{}
	}
	return s
}

// put places a file (non-empty size) into the stub.
func (s *stubS3) put(key string, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = stubObject{size: size, modTime: stubModTime}
}

func (s *stubS3) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok
}

func (s *stubS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p := strings.TrimPrefix(r.URL.Path, "/")
	var key string
	if i := strings.Index(p, "/"); i >= 0 {
		key = p[i+1:]
	}

	if key == "" {
		// Bucket-level. The HeadBucket startup probe is a HEAD with no key;
		// a GET with no key is a bucket-scoped ListObjectsV2; a POST
		// carrying ?delete is the batch delete.
		if r.Method == http.MethodGet {
			s.list(w, r)
			return
		}
		if r.Method == http.MethodPost && r.URL.Query().Has("delete") {
			s.batchDelete(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.createMultipart(w, r, key)
	case r.Method == http.MethodPut && q.Has("partNumber") && q.Has("uploadId"):
		s.uploadPart(w, r, key, q)
	case r.Method == http.MethodGet && q.Has("uploadId"):
		s.listParts(w, r, key, q)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		s.completeMultipart(w, r, key, q)
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		s.abortMultipart(w, q)
	case r.Method == http.MethodPut && r.Header.Get("x-amz-copy-source") != "":
		s.copyObject(w, r, key)
	case r.Method == http.MethodDelete:
		// Deleting a missing key succeeds on S3 — it is the idempotency
		// the abort path leans on.
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodHead:
		s.headObject(w, key)
	case r.Method == http.MethodGet && q.Has("list-type"):
		s.list(w, r)
	case r.Method == http.MethodGet:
		s.getObject(w, r, key)
	case r.Method == http.MethodPut:
		s.putObject(w, r, key)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *stubS3) headObject(w http.ResponseWriter, key string) {
	obj, ok := s.objects[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(obj.size, 10))
	w.Header().Set("Last-Modified", obj.modTime.Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

func (s *stubS3) getObject(w http.ResponseWriter, r *http.Request, key string) {
	obj, ok := s.objects[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(obj.size, 10))
	w.Header().Set("Last-Modified", obj.modTime.Format(http.TimeFormat))
	// A per-request override, as providers honour on presigned GETs.
	if disp := r.URL.Query().Get("response-content-disposition"); disp != "" {
		w.Header().Set("Content-Disposition", disp)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(obj.body)
}

func (s *stubS3) putObject(w http.ResponseWriter, r *http.Request, key string) {
	body, _ := io.ReadAll(r.Body)
	s.objects[key] = stubObject{size: int64(len(body)), modTime: stubModTime, body: body}
	// Echo a flexible checksum if the SDK sent one, as providers do.
	if v := r.Header.Get("x-amz-checksum-crc32"); v != "" {
		w.Header().Set("x-amz-checksum-crc32", v)
	}
	w.Header().Set("ETag", `"stub-etag"`)
	w.WriteHeader(http.StatusOK)
}

// createMultipart opens an upload and hands back its ID.
func (s *stubS3) createMultipart(w http.ResponseWriter, r *http.Request, key string) {
	s.nextUpload++
	id := fmt.Sprintf("upload-%d", s.nextUpload)
	s.uploads[id] = &stubUpload{key: key, parts: make(map[int][]byte)}
	writeXML(w, struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		UploadID string   `xml:"UploadId"`
	}{Bucket: "test-bucket", Key: key, UploadID: id})
}

// uploadPart records one part's bytes under its number.
func (s *stubS3) uploadPart(w http.ResponseWriter, r *http.Request, key string, q url.Values) {
	up, ok := s.uploads[q.Get("uploadId")]
	if !ok || up.key != key {
		writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "the upload does not exist")
		return
	}
	n, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil || n < 1 {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "bad part number")
		return
	}
	body, _ := io.ReadAll(r.Body)
	up.parts[n] = body
	w.Header().Set("ETag", fmt.Sprintf(`"etag-%s-%d"`, q.Get("uploadId"), n))
	w.WriteHeader(http.StatusOK)
}

// listParts reports the received parts in number order.
func (s *stubS3) listParts(w http.ResponseWriter, r *http.Request, key string, q url.Values) {
	up, ok := s.uploads[q.Get("uploadId")]
	if !ok || up.key != key {
		writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "the upload does not exist")
		return
	}
	numbers := make([]int, 0, len(up.parts))
	for n := range up.parts {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	type part struct {
		Number       int    `xml:"PartNumber"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
	}
	out := struct {
		XMLName     xml.Name `xml:"ListPartsResult"`
		Bucket      string   `xml:"Bucket"`
		Key         string   `xml:"Key"`
		UploadID    string   `xml:"UploadId"`
		MaxParts    int      `xml:"MaxParts"`
		IsTruncated bool     `xml:"IsTruncated"`
		Parts       []part   `xml:"Part"`
	}{Bucket: "test-bucket", Key: key, UploadID: q.Get("uploadId"), MaxParts: 1000}
	for _, n := range numbers {
		out.Parts = append(out.Parts, part{
			Number:       n,
			LastModified: stubModTime.UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         fmt.Sprintf(`"etag-%s-%d"`, q.Get("uploadId"), n),
			Size:         int64(len(up.parts[n])),
		})
	}
	writeXML(w, out)
}

// completeMultipart assembles the parts in the order the client lists them —
// which is what S3 does — and publishes the object.
func (s *stubS3) completeMultipart(w http.ResponseWriter, r *http.Request, key string, q url.Values) {
	up, ok := s.uploads[q.Get("uploadId")]
	if !ok || up.key != key {
		writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "the upload does not exist")
		return
	}
	var req struct {
		Parts []struct {
			Number int    `xml:"PartNumber"`
			ETag   string `xml:"ETag"`
		} `xml:"Part"`
	}
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "cannot parse the completion")
		return
	}
	if len(req.Parts) == 0 {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "no parts to complete")
		return
	}
	var body []byte
	for _, p := range req.Parts {
		part, ok := up.parts[p.Number]
		if !ok {
			writeS3Error(w, http.StatusBadRequest, "InvalidPart", "a listed part was never uploaded")
			return
		}
		body = append(body, part...)
	}
	delete(s.uploads, q.Get("uploadId"))
	s.objects[key] = stubObject{size: int64(len(body)), modTime: stubModTime, body: body}
	writeXML(w, struct {
		XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
		Bucket  string   `xml:"Bucket"`
		Key     string   `xml:"Key"`
		ETag    string   `xml:"ETag"`
	}{Bucket: "test-bucket", Key: key, ETag: `"stub-complete-etag"`})
}

// abortMultipart discards an upload; aborting a vanished one succeeds, which
// is the idempotency the backend leans on.
func (s *stubS3) abortMultipart(w http.ResponseWriter, q url.Values) {
	delete(s.uploads, q.Get("uploadId"))
	w.WriteHeader(http.StatusNoContent)
}

// copyObject duplicates an object server-side.
func (s *stubS3) copyObject(w http.ResponseWriter, r *http.Request, key string) {
	src := strings.TrimPrefix(r.Header.Get("x-amz-copy-source"), "test-bucket/")
	if unescaped, err := url.PathUnescape(src); err == nil {
		src = unescaped
	}
	obj, ok := s.objects[src]
	if !ok {
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "the source does not exist")
		return
	}
	s.objects[key] = obj
	writeXML(w, struct {
		XMLName      xml.Name `xml:"CopyObjectResult"`
		LastModified string   `xml:"LastModified"`
		ETag         string   `xml:"ETag"`
	}{
		LastModified: stubModTime.UTC().Format("2006-01-02T15:04:05.000Z"),
		ETag:         `"stub-copy-etag"`,
	})
}

// batchDelete removes the listed keys; missing ones are simply absent from
// the answer, exactly as providers do.
func (s *stubS3) batchDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Objects []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "cannot parse the deletion")
		return
	}
	for _, o := range req.Objects {
		delete(s.objects, o.Key)
	}
	writeXML(w, struct {
		XMLName xml.Name `xml:"DeleteResult"`
	}{})
}

func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>`)
	_ = xml.NewEncoder(w).Encode(v)
}

func writeS3Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, message)
}

// list answers ListObjectsV2: delimiter grouping, continuation tokens, and a
// server-side page cap so the client's pagination loop is exercised no matter
// what MaxKeys it asks for.
func (s *stubS3) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix, delimiter := q.Get("prefix"), q.Get("delimiter")
	maxKeys := 1000
	if v := q.Get("max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxKeys = n
		}
	}
	if s.pageSize > 0 && s.pageSize < maxKeys {
		maxKeys = s.pageSize
	}
	after := q.Get("continuation-token")

	keys := make([]string, 0, len(s.objects))
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	type content struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
		StorageClass string `xml:"StorageClass"`
	}
	type commonPrefix struct {
		Prefix string `xml:"Prefix"`
	}
	type result struct {
		XMLName               xml.Name       `xml:"ListBucketResult"`
		Name                  string         `xml:"Name"`
		Prefix                string         `xml:"Prefix"`
		KeyCount              int            `xml:"KeyCount"`
		MaxKeys               int            `xml:"MaxKeys"`
		Delimiter             string         `xml:"Delimiter,omitempty"`
		IsTruncated           bool           `xml:"IsTruncated"`
		NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
		Contents              []content      `xml:"Contents"`
		CommonPrefixes        []commonPrefix `xml:"CommonPrefixes"`
	}

	var out result
	out.Name = "test-bucket"
	out.Prefix = prefix
	out.MaxKeys = maxKeys
	out.Delimiter = delimiter

	seenPrefix := make(map[string]bool)
	count, last := 0, ""
	for _, k := range keys {
		if after != "" && k <= after {
			continue
		}
		rest := k[len(prefix):]
		if delimiter != "" {
			if i := strings.Index(rest, delimiter); i >= 0 {
				cp := prefix + rest[:i+len(delimiter)]
				if !seenPrefix[cp] {
					if count >= maxKeys {
						out.IsTruncated, out.NextContinuationToken = true, last
						break
					}
					seenPrefix[cp] = true
					out.CommonPrefixes = append(out.CommonPrefixes, commonPrefix{Prefix: cp})
					count++
				}
				continue
			}
		}
		if count >= maxKeys {
			out.IsTruncated, out.NextContinuationToken = true, last
			break
		}
		obj := s.objects[k]
		out.Contents = append(out.Contents, content{
			Key:          k,
			LastModified: obj.modTime.UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         `"stub-etag"`,
			Size:         obj.size,
			StorageClass: "STANDARD",
		})
		count++
		last = k
	}
	out.KeyCount = count

	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>`)
	_ = xml.NewEncoder(w).Encode(out)
}

// newTestS3 opens a backend against a fresh stub server.
func newTestS3(t *testing.T, stub *stubS3, mutate func(*S3Config)) *S3 {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)

	cfg := S3Config{
		Bucket:          "test-bucket",
		Region:          "us-east-1",
		Endpoint:        srv.URL,
		ForcePathStyle:  true,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := OpenS3(cfg)
	if err != nil {
		t.Fatalf("OpenS3: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenS3RequiresBucketAndRegion(t *testing.T) {
	if _, err := OpenS3(S3Config{Region: "us-east-1"}); err == nil {
		t.Error("OpenS3 without a bucket succeeded")
	}
	if _, err := OpenS3(S3Config{Bucket: "b"}); err == nil {
		t.Error("OpenS3 without a region succeeded")
	}
}

func TestOpenS3RefusesUnreachableBucket(t *testing.T) {
	srv := httptest.NewServer(newStubS3())
	url := srv.URL
	srv.Close() // nothing listens any more

	_, err := OpenS3(S3Config{
		Bucket: "test-bucket", Region: "us-east-1", Endpoint: url,
		ForcePathStyle: true, AccessKeyID: "test", SecretAccessKey: "test",
	})
	if err == nil {
		t.Fatal("OpenS3 against a dead endpoint succeeded")
	}
}

func TestS3Stat(t *testing.T) {
	stub := newStubS3("docs/", ".zefile-trash/ignored")
	stub.put("docs/", 0) // the marker carries the directory's mtime
	stub.put("report.pdf", 5)
	stub.put("docs/deep.txt", 7)
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	root, err := s.Stat(ctx, MustParsePath("/"))
	if err != nil || !root.IsDir {
		t.Fatalf("Stat(/) = %+v, %v; want a directory", root, err)
	}

	file, err := s.Stat(ctx, MustParsePath("/report.pdf"))
	if err != nil {
		t.Fatalf("Stat(/report.pdf): %v", err)
	}
	if file.IsDir || file.Size != 5 || !file.ModTime.Equal(stubModTime) {
		t.Errorf("Stat(/report.pdf) = %+v, want a 5-byte file at %v", file, stubModTime)
	}

	dir, err := s.Stat(ctx, MustParsePath("/docs"))
	if err != nil || !dir.IsDir {
		t.Fatalf("Stat(/docs) = %+v, %v; want a directory", dir, err)
	}
	if !dir.ModTime.Equal(stubModTime) {
		t.Errorf("Stat(/docs) ModTime = %v, want the marker's %v", dir.ModTime, stubModTime)
	}

	// A directory with children but no marker exists too.
	stub.put("implicit/leaf.txt", 1)
	implicit, err := s.Stat(ctx, MustParsePath("/implicit"))
	if err != nil || !implicit.IsDir {
		t.Fatalf("Stat(/implicit) = %+v, %v; want a directory", implicit, err)
	}

	if _, err := s.Stat(ctx, MustParsePath("/phantom")); !errors.Is(err, ErrNotExist) {
		t.Errorf("Stat(/phantom) = %v, want ErrNotExist", err)
	}
	if _, err := s.Stat(ctx, MustParsePath("/.zefile-trash")); !errors.Is(err, ErrReserved) {
		t.Errorf("Stat reserved = %v, want ErrReserved", err)
	}
}

func TestS3List(t *testing.T) {
	stub := newStubS3("docs/", "docs/deep.txt", ".zefile-trash/x", "report.pdf")
	stub.put("report.pdf", 5)
	stub.put("docs/deep.txt", 7)
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	entries, err := s.List(ctx, MustParsePath("/"))
	if err != nil {
		t.Fatalf("List(/): %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name] = e.IsDir
	}
	if len(entries) != 2 || !got["docs"] || got["report.pdf"] {
		// docs must be a directory (true), report.pdf a file (false).
		t.Fatalf("List(/) = %+v, want docs(dir) and report.pdf(file), reserved hidden", entries)
	}

	inner, err := s.List(ctx, MustParsePath("/docs"))
	if err != nil || len(inner) != 1 || inner[0].Name != "deep.txt" || inner[0].Size != 7 {
		t.Fatalf("List(/docs) = %+v, %v; want one 7-byte deep.txt", inner, err)
	}

	if _, err := s.List(ctx, MustParsePath("/report.pdf")); !errors.Is(err, ErrNotDir) {
		t.Errorf("List of a file = %v, want ErrNotDir", err)
	}
	if _, err := s.List(ctx, MustParsePath("/phantom")); !errors.Is(err, ErrNotExist) {
		t.Errorf("List of a missing dir = %v, want ErrNotExist", err)
	}
}

func TestS3ListPaginates(t *testing.T) {
	stub := newStubS3()
	for i := 0; i < 7; i++ {
		stub.put(fmt.Sprintf("many/f%d.txt", i), int64(i))
	}
	stub.pageSize = 2 // the client asks for 1000; the server hands back 2
	s := newTestS3(t, stub, nil)

	entries, err := s.List(context.Background(), MustParsePath("/many"))
	if err != nil {
		t.Fatalf("List(/many): %v", err)
	}
	if len(entries) != 7 {
		t.Fatalf("List(/many) returned %d entries, want all 7 across pages", len(entries))
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	sort.Strings(names)
	for i, want := range []string{"f0.txt", "f1.txt", "f2.txt", "f3.txt", "f4.txt", "f5.txt", "f6.txt"} {
		if names[i] != want {
			t.Fatalf("entries = %v, want %s at %d", names, want, i)
		}
	}
}

func TestS3ListHonoursGuard(t *testing.T) {
	stub := newStubS3("public.txt", "secret.txt")
	s := newTestS3(t, stub, func(cfg *S3Config) {
		cfg.Guard = denyGuard{deny: "/secret.txt"}
	})
	ctx := context.Background()

	if _, err := s.Stat(ctx, MustParsePath("/secret.txt")); !errors.Is(err, ErrPermission) {
		t.Errorf("Stat of a denied path = %v, want ErrPermission", err)
	}

	entries, err := s.List(ctx, MustParsePath("/"))
	if err != nil {
		t.Fatalf("List(/): %v", err)
	}
	for _, e := range entries {
		if e.Name == "secret.txt" {
			t.Fatalf("List leaked a denied entry: %+v", entries)
		}
	}
	if len(entries) != 1 || entries[0].Name != "public.txt" {
		t.Fatalf("List(/) = %+v, want only public.txt", entries)
	}
}

func TestS3Mkdir(t *testing.T) {
	stub := newStubS3()
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	if err := s.Mkdir(ctx, MustParsePath("/")); !errors.Is(err, ErrExist) {
		t.Errorf("Mkdir(/) = %v, want ErrExist", err)
	}
	if err := s.Mkdir(ctx, MustParsePath("/missing/child")); !errors.Is(err, ErrNotExist) {
		t.Errorf("Mkdir under a missing parent = %v, want ErrNotExist", err)
	}

	if err := s.Mkdir(ctx, MustParsePath("/newdir")); err != nil {
		t.Fatalf("Mkdir(/newdir): %v", err)
	}
	if !stub.has("newdir/") {
		t.Fatal("Mkdir did not write a directory marker")
	}
	if err := s.Mkdir(ctx, MustParsePath("/newdir")); !errors.Is(err, ErrExist) {
		t.Errorf("second Mkdir = %v, want ErrExist", err)
	}
}

func TestS3MkdirReadOnly(t *testing.T) {
	s := newTestS3(t, newStubS3(), func(cfg *S3Config) { cfg.ReadOnly = true })
	if err := s.Mkdir(context.Background(), MustParsePath("/newdir")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Mkdir in read-only mode = %v, want ErrReadOnly", err)
	}
	info, err := s.Space(context.Background())
	if err != nil || !info.ReadOnly {
		t.Errorf("Space = %+v, %v; want ReadOnly", info, err)
	}
}

func TestS3MkdirAll(t *testing.T) {
	stub := newStubS3("file.txt")
	stub.put("file.txt", 3)
	s := newTestS3(t, stub, nil)
	ctx := context.Background()

	if err := s.MkdirAll(ctx, MustParsePath("/a/b/c")); err != nil {
		t.Fatalf("MkdirAll(/a/b/c): %v", err)
	}
	for _, marker := range []string{"a/", "a/b/", "a/b/c/"} {
		if !stub.has(marker) {
			t.Errorf("MkdirAll did not create marker %q", marker)
		}
	}
	if err := s.MkdirAll(ctx, MustParsePath("/a/b/c")); err != nil {
		t.Errorf("MkdirAll over an existing tree = %v, want nil", err)
	}
	if err := s.MkdirAll(ctx, MustParsePath("/file.txt/sub")); !errors.Is(err, ErrNotDir) {
		t.Errorf("MkdirAll under a file = %v, want ErrNotDir", err)
	}
	if err := s.MkdirAll(ctx, MustParsePath("/")); err != nil {
		t.Errorf("MkdirAll(/) = %v, want nil", err)
	}
}

func TestS3RefusedOperations(t *testing.T) {
	s := newTestS3(t, newStubS3(), nil)
	ctx := context.Background()
	p := MustParsePath("/x.bin")

	if _, err := s.Create(ctx, p); !errors.Is(err, ErrNotSupported) {
		t.Errorf("Create = %v, want ErrNotSupported", err)
	}
	if _, err := s.Open(ctx, p); !errors.Is(err, ErrNotSupported) {
		t.Errorf("Open = %v, want ErrNotSupported", err)
	}
	// Copy, move and delete answer ErrNotExist on a missing path.
	if err := s.Remove(ctx, p); !errors.Is(err, ErrNotExist) {
		t.Errorf("Remove = %v, want ErrNotExist", err)
	}
	if err := s.RemoveAll(ctx, p); err != nil {
		t.Errorf("RemoveAll = %v, want nil (idempotent)", err)
	}
	if err := s.Move(ctx, p, MustParsePath("/y.bin")); !errors.Is(err, ErrNotExist) {
		t.Errorf("Move = %v, want ErrNotExist", err)
	}
	if err := s.Copy(ctx, p, MustParsePath("/z.bin")); !errors.Is(err, ErrNotExist) {
		t.Errorf("Copy = %v, want ErrNotExist", err)
	}

	info, err := s.Space(ctx)
	if err != nil || info.ReadOnly {
		t.Errorf("Space = %+v, %v; want writable with unknown capacity", info, err)
	}
}

func TestS3PrefixScoping(t *testing.T) {
	stub := newStubS3("data/inside.txt", "other.txt")
	stub.put("data/inside.txt", 9)
	s := newTestS3(t, stub, func(cfg *S3Config) { cfg.Prefix = "/data/" })
	ctx := context.Background()

	// The tree is scoped: what the caller sees as /inside.txt is the
	// prefixed key, and nothing outside the prefix is visible.
	file, err := s.Stat(ctx, MustParsePath("/inside.txt"))
	if err != nil || file.Size != 9 {
		t.Fatalf("Stat(/inside.txt) = %+v, %v; want the prefixed 9-byte object", file, err)
	}
	if _, err := s.Stat(ctx, MustParsePath("/other.txt")); !errors.Is(err, ErrNotExist) {
		t.Errorf("Stat outside the prefix = %v, want ErrNotExist", err)
	}

	entries, err := s.List(ctx, MustParsePath("/"))
	if err != nil || len(entries) != 1 || entries[0].Name != "inside.txt" {
		t.Fatalf("List(/) under a prefix = %+v, %v; want only inside.txt", entries, err)
	}

	if err := s.Mkdir(ctx, MustParsePath("/newdir")); err != nil {
		t.Fatalf("Mkdir under a prefix: %v", err)
	}
	if !stub.has("data/newdir/") {
		t.Error("Mkdir wrote outside the prefix")
	}
}

// denyGuard refuses one path and permits everything else.
type denyGuard struct{ deny string }

func (g denyGuard) Authorize(_ context.Context, _ Op, p Path) error {
	if p.String() == g.deny {
		return ErrPermission
	}
	return nil
}

func (g denyGuard) Permitted(_ context.Context, _ Op, paths []Path) ([]bool, error) {
	verdicts := make([]bool, len(paths))
	for i, p := range paths {
		verdicts[i] = p.String() != g.deny
	}
	return verdicts, nil
}
