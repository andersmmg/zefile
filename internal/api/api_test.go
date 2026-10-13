package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krishna2206/zefile/internal/acl"
	"github.com/krishna2206/zefile/internal/api"
	"github.com/krishna2206/zefile/internal/audit"
	"github.com/krishna2206/zefile/internal/auth"
	"github.com/krishna2206/zefile/internal/checksum"
	entrycolor "github.com/krishna2206/zefile/internal/color"
	"github.com/krishna2206/zefile/internal/db"
	"github.com/krishna2206/zefile/internal/job"
	"github.com/krishna2206/zefile/internal/quota"
	"github.com/krishna2206/zefile/internal/share"
	"github.com/krishna2206/zefile/internal/storage"
	"github.com/krishna2206/zefile/internal/thumb"
	"github.com/krishna2206/zefile/internal/trash"
	"github.com/krishna2206/zefile/internal/upload"
)

// The tests here drive the server the way a client does: over HTTP, through the
// real router, storage layer and ACL engine. This is the lot's completion
// criterion — the tree navigable and manipulable from outside the process.

type client struct {
	t       *testing.T
	server  *httptest.Server
	token   string
	root    string
	auth    *auth.Service
	aclEngi *acl.Engine
	quota   *quota.Service
}

func newClient(t *testing.T) *client {
	t.Helper()

	database, err := db.Open(t.Context(), db.Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	engine := acl.New(database)
	root := t.TempDir()
	fs, err := storage.Open(storage.Config{Root: root, Guard: engine, Reserve: 1})
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	// Cheap hashing: these tests exercise routing and wiring, not Argon2.
	service := auth.New(database, auth.WithParams(auth.Params{
		Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	}))

	// A running worker, so a background copy actually completes in tests.
	jobs := job.New(database, job.WithPollInterval(20*time.Millisecond))
	jobs.Register(job.TypeCopy, func(ctx context.Context, payload string, report func(done, total int64)) error {
		var p job.CopyPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return err
		}
		subject, err := engine.LoadSubject(ctx, p.UserID, p.IsAdmin)
		if err != nil {
			return err
		}
		ctx = acl.NewContext(ctx, subject)
		from, err := storage.ParsePath(p.From)
		if err != nil {
			return err
		}
		to, err := storage.ParsePath(p.To)
		if err != nil {
			return err
		}
		if err := fs.CopyTree(ctx, from, to, report); err != nil {
			return err
		}
		return engine.SetOwner(ctx, to, p.UserID)
	})
	checksums := checksum.New(database, fs)
	jobs.Register(job.TypeChecksum, func(ctx context.Context, payload string, _ func(done, total int64)) error {
		var p checksum.Payload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return err
		}
		subject, err := engine.LoadSubject(ctx, p.UserID, p.IsAdmin)
		if err != nil {
			return err
		}
		ctx = acl.NewContext(ctx, subject)
		path, err := storage.ParsePath(p.Path)
		if err != nil {
			return err
		}
		_, err = checksums.Compute(ctx, path)
		return err
	})
	workerCtx, stopWorker := context.WithCancel(context.Background())
	t.Cleanup(stopWorker)
	go jobs.Run(workerCtx)

	quotaSvc := quota.New(fs, 0, engine, service)
	srv := httptest.NewServer(api.New(api.Options{
		FS: fs, Auth: service, ACL: engine,
		Quota:   quotaSvc,
		Uploads: upload.New(database, fs),
		Trash:   trash.New(database, fs),
		Shares: share.New(database, fs, share.GuardFunc(func(ctx context.Context, p storage.Path) (bool, error) {
			return engine.Allows(ctx, acl.PermShare, p)
		})),
		Jobs:          jobs,
		Checksums:     checksums,
		Audit:         audit.New(database),
		ContentBase:   "https://content.example",
		SecureCookies: false,
		Colors:        entrycolor.New(database),
	}).Handler())
	t.Cleanup(srv.Close)

	return &client{t: t, server: srv, root: root, auth: service, aclEngi: engine, quota: quotaSvc}
}

// do issues a request, attaching the bearer token once signed in.
func (c *client) do(method, path string, body any) (*http.Response, []byte) {
	c.t.Helper()

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("encode body: %v", err)
		}
		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(c.t.Context(), method, c.server.URL+path, payload)
	if err != nil {
		c.t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.server.Client().Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	return resp, raw
}

// setUp completes first-run setup and keeps the returned token.
func (c *client) setUp() {
	c.t.Helper()

	// Startup mints the first setup token rather than an endpoint, so the test
	// does the same thing the binary does.
	token, err := c.auth.IssueSetupToken(c.t.Context())
	if err != nil {
		c.t.Fatalf("IssueSetupToken: %v", err)
	}

	resp, raw := c.do(http.MethodPost, "/api/v1/setup", map[string]string{
		"token":    token,
		"username": "krishna",
		"password": "correct horse battery",
	})
	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("setup returned %d: %s", resp.StatusCode, raw)
	}

	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		c.t.Fatalf("decode setup response: %v", err)
	}
	c.token = session.Token
}

func decode[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

type problem struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Title  string `json:"title"`
}

type entry struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
	Color string `json:"color,omitempty"`
}

type listing struct {
	Path    string  `json:"path"`
	Entries []entry `json:"entries"`
}

func TestHealth(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	resp, raw := c.do(http.MethodGet, "/healthz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
}

func TestUnauthenticatedIsRefused(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	for _, path := range []string{"/api/v1/fs", "/api/v1/fs/stat?path=/", "/api/v1/auth/me"} {
		resp, raw := c.do(http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s returned %d, want 401: %s", path, resp.StatusCode, raw)
		}
		if got := decode[problem](t, raw); got.Code != api.CodeUnauthenticated {
			t.Errorf("%s code = %q, want %q", path, got.Code, api.CodeUnauthenticated)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s content type = %q, want application/problem+json", path, ct)
		}
	}
}

// TestFullLifecycle walks the tree the way a client does: sign in, create,
// list, copy, move, delete. It is the lot's completion criterion.
func TestFullLifecycle(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	// An empty instance lists an empty root rather than failing.
	resp, raw := c.do(http.MethodGet, "/api/v1/fs", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list root: %d %s", resp.StatusCode, raw)
	}
	if got := decode[listing](t, raw); len(got.Entries) != 0 || got.Path != "/" {
		t.Fatalf("fresh root = %+v, want an empty listing at /", got)
	}

	// Nested creation in one call.
	resp, raw = c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/jeux/steam"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mkdir: %d %s", resp.StatusCode, raw)
	}

	// A file placed out of band must appear: the filesystem is the authority.
	if err := os.WriteFile(filepath.Join(c.root, "jeux", "steam", "doom.iso"), []byte("payload"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	resp, raw = c.do(http.MethodGet, "/api/v1/fs?path=/jeux/steam", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %s", resp.StatusCode, raw)
	}
	entries := decode[listing](t, raw).Entries
	if len(entries) != 1 || entries[0].Name != "doom.iso" || entries[0].Size != 7 {
		t.Fatalf("listing = %+v, want the file dropped in over SSH", entries)
	}

	resp, raw = c.do(http.MethodPost, "/api/v1/fs/copy",
		map[string]string{"from": "/jeux/steam/doom.iso", "to": "/jeux/steam/doom-copie.iso"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("copy: %d %s", resp.StatusCode, raw)
	}

	resp, raw = c.do(http.MethodPost, "/api/v1/fs/move",
		map[string]string{"from": "/jeux/steam/doom-copie.iso", "to": "/jeux/doom2.iso"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("move: %d %s", resp.StatusCode, raw)
	}
	if got := decode[entry](t, raw); got.Path != "/jeux/doom2.iso" {
		t.Errorf("move returned %q", got.Path)
	}

	resp, raw = c.do(http.MethodDelete, "/api/v1/fs?path=/jeux/doom2.iso", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d %s", resp.StatusCode, raw)
	}
	if _, err := os.Stat(filepath.Join(c.root, "jeux", "doom2.iso")); !os.IsNotExist(err) {
		t.Error("the file survived the delete")
	}
}

type trashItem struct {
	ID           int64  `json:"id"`
	OriginalPath string `json:"original_path"`
	IsDir        bool   `json:"is_dir"`
}

type trashList struct {
	Items []trashItem `json:"items"`
}

type jobEntry struct {
	ID int64 `json:"id"`
}

type jobList struct {
	Jobs []jobEntry `json:"jobs"`
}

func TestDeleteMovesToTrashAndRestores(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	// A non-empty directory: deleting it moves the whole tree at once, so the
	// old "recursive" flag no longer means anything.
	if _, raw := c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/dossier/sous"}); raw == nil {
		t.Fatal("mkdir returned nothing")
	}

	resp, raw := c.do(http.MethodDelete, "/api/v1/fs?path=/dossier", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", resp.StatusCode, raw)
	}

	// Gone from the listing...
	if _, raw := c.do(http.MethodGet, "/api/v1/fs?path=/", nil); strings.Contains(string(raw), "dossier") {
		t.Fatalf("deleted folder still listed: %s", raw)
	}

	// ...but sitting in the trash, remembering where it came from.
	_, raw = c.do(http.MethodGet, "/api/v1/trash", nil)
	list := decode[trashList](t, raw)
	if len(list.Items) != 1 || list.Items[0].OriginalPath != "/dossier" || !list.Items[0].IsDir {
		t.Fatalf("trash = %+v", list.Items)
	}
	id := list.Items[0].ID

	// Restoring puts it back where it was and empties the trash.
	resp, raw = c.do(http.MethodPost, fmt.Sprintf("/api/v1/trash/%d/restore", id), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("restore = %d: %s", resp.StatusCode, raw)
	}
	if _, raw := c.do(http.MethodGet, "/api/v1/fs?path=/", nil); !strings.Contains(string(raw), "dossier") {
		t.Fatalf("restored folder not listed: %s", raw)
	}
	if _, raw := c.do(http.MethodGet, "/api/v1/trash", nil); len(decode[trashList](t, raw).Items) != 0 {
		t.Fatalf("trash not empty after restore: %s", raw)
	}
}

type shareItem struct {
	ID   int64  `json:"id"`
	URL  string `json:"url"`
	Name string `json:"name"`
}

type shareList struct {
	Shares []shareItem `json:"shares"`
}

func TestShareLifecycle(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	if err := os.WriteFile(filepath.Join(c.root, "report.pdf"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// Create a link to the file.
	resp, raw := c.do(http.MethodPost, "/api/v1/shares", map[string]any{"path": "/report.pdf"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d: %s", resp.StatusCode, raw)
	}
	sh := decode[shareItem](t, raw)
	prefix := "https://content.example/s/"
	if !strings.HasPrefix(sh.URL, prefix) || len(sh.URL)-len(prefix) < 20 {
		t.Fatalf("share url = %q, want a /s/<token> link", sh.URL)
	}
	if sh.Name != "report.pdf" {
		t.Fatalf("name = %q", sh.Name)
	}

	// A folder can be shared, but not with a password yet (that would store a
	// password the browse flow never checks).
	c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/folder"})
	if resp2, _ := c.do(http.MethodPost, "/api/v1/shares", map[string]any{"path": "/folder", "password": "x"}); resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("password folder share = %d, want 400", resp2.StatusCode)
	}

	// It shows up in the owner's list, without the token.
	_, raw = c.do(http.MethodGet, "/api/v1/shares", nil)
	list := decode[shareList](t, raw)
	if len(list.Shares) != 1 || list.Shares[0].ID != sh.ID {
		t.Fatalf("list = %+v", list.Shares)
	}
	if list.Shares[0].URL != "" {
		t.Fatalf("list leaked a token: %q", list.Shares[0].URL)
	}

	// Revoke it; the list empties, and revoking again is a 404.
	if resp3, _ := c.do(http.MethodDelete, fmt.Sprintf("/api/v1/shares/%d", sh.ID), nil); resp3.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke = %d", resp3.StatusCode)
	}
	if _, raw := c.do(http.MethodGet, "/api/v1/shares", nil); len(decode[shareList](t, raw).Shares) != 0 {
		t.Fatalf("list not empty after revoke: %s", raw)
	}
	if resp4, _ := c.do(http.MethodDelete, fmt.Sprintf("/api/v1/shares/%d", sh.ID), nil); resp4.StatusCode != http.StatusNotFound {
		t.Fatalf("re-revoke = %d, want 404", resp4.StatusCode)
	}
}

func TestFolderShareCreate(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/album"})
	resp, raw := c.do(http.MethodPost, "/api/v1/shares", map[string]any{"path": "/album"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("folder share = %d: %s", resp.StatusCode, raw)
	}
	if sh := decode[shareItem](t, raw); !strings.HasPrefix(sh.URL, "https://content.example/s/") {
		t.Fatalf("folder share url = %q", sh.URL)
	}
}

func TestThumbnailResizesImages(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	// A 800x600 image written straight into the storage tree.
	img := image.NewRGBA(image.Rect(0, 0, 800, 600))
	for x := 0; x < 800; x++ {
		img.Set(x, 0, color.RGBA{R: uint8(x % 256), G: 120, B: 200, A: 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	if err := os.WriteFile(filepath.Join(c.root, "pic.png"), buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write png: %v", err)
	}

	resp, raw := c.do(http.MethodGet, "/api/v1/fs/thumb?path=/pic.png&s=256", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("thumb = %d: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("content-type = %q, want image/jpeg", ct)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode thumbnail: %v", err)
	}
	// Capped at 256 on the long side; the landscape source scales to 256x192.
	if cfg.Width != 256 || cfg.Height != 192 {
		t.Fatalf("thumbnail is %dx%d, want 256x192", cfg.Width, cfg.Height)
	}

	// A non-image is refused rather than streamed as if it were one.
	if err := os.WriteFile(filepath.Join(c.root, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write txt: %v", err)
	}
	resp2, _ := c.do(http.MethodGet, "/api/v1/fs/thumb?path=/notes.txt", nil)
	if resp2.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("txt thumb = %d, want 415", resp2.StatusCode)
	}
}

func TestTrashPurgeRemovesForGood(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/jetable"})
	c.do(http.MethodDelete, "/api/v1/fs?path=/jetable", nil)

	_, raw := c.do(http.MethodGet, "/api/v1/trash", nil)
	list := decode[trashList](t, raw)
	if len(list.Items) != 1 {
		t.Fatalf("trash = %+v", list.Items)
	}

	resp, raw := c.do(http.MethodDelete, fmt.Sprintf("/api/v1/trash/%d", list.Items[0].ID), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("purge = %d: %s", resp.StatusCode, raw)
	}
	if _, raw := c.do(http.MethodGet, "/api/v1/trash", nil); len(decode[trashList](t, raw).Items) != 0 {
		t.Fatalf("trash not empty after purge: %s", raw)
	}
}

func TestPathValidationReachesTheClient(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	for _, bad := range []string{"/../etc/passwd", "relative", "/a//b", "/a/./b"} {
		resp, raw := c.do(http.MethodGet, "/api/v1/fs/stat?path="+url.QueryEscape(bad), nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q returned %d, want 400: %s", bad, resp.StatusCode, raw)
			continue
		}
		if got := decode[problem](t, raw); got.Code != api.CodeInvalidPath {
			t.Errorf("%q code = %q, want %q", bad, got.Code, api.CodeInvalidPath)
		}
	}
}

// TestLogoutEndsTheSessionImmediately is the File Browser defect checked at the
// HTTP boundary rather than in the domain: the very next request must fail.
func TestLogoutEndsTheSessionImmediately(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	if resp, raw := c.do(http.MethodGet, "/api/v1/auth/me", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("me before logout: %d %s", resp.StatusCode, raw)
	}
	if resp, raw := c.do(http.MethodPost, "/api/v1/auth/logout", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d %s", resp.StatusCode, raw)
	}

	resp, raw := c.do(http.MethodGet, "/api/v1/auth/me", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the token still worked after logout: %d %s", resp.StatusCode, raw)
	}
}

func TestLoginAndCookie(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	resp, raw := c.do(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"username": "krishna", "password": "correct horse battery"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d %s", resp.StatusCode, raw)
	}

	var found bool
	for _, cookie := range resp.Cookies() {
		if cookie.Name != auth.SessionCookieName {
			continue
		}
		found = true
		if !cookie.HttpOnly {
			t.Error("the session cookie is readable by scripts")
		}
		if cookie.SameSite != http.SameSiteLaxMode {
			t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
		}
	}
	if !found {
		t.Fatal("login set no session cookie")
	}

	if resp, raw = c.do(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"username": "krishna", "password": "wrong"}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d, want 401: %s", resp.StatusCode, raw)
	}
	if got := decode[problem](t, raw); got.Code != api.CodeInvalidCredentials {
		t.Errorf("code = %q, want %q", got.Code, api.CodeInvalidCredentials)
	}
}

func TestMalformedBodyIsRejected(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	// An unknown field is refused rather than ignored, so a client with a typo
	// is told instead of silently getting the default.
	resp, raw := c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"chemin": "/x"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field = %d, want 400: %s", resp.StatusCode, raw)
	}
}

func TestSearchWalksTheTree(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/photos/2024"})
	for _, f := range []struct{ rel, data string }{
		{"photos/2024/beach-sunset.jpg", "x"},
		{"photos/vacation-notes.txt", "y"},
		{"report.pdf", "z"},
	} {
		if err := os.WriteFile(filepath.Join(c.root, filepath.FromSlash(f.rel)), []byte(f.data), 0o644); err != nil {
			t.Fatalf("seed %s: %v", f.rel, err)
		}
	}

	type searchResp struct {
		Query     string  `json:"query"`
		Results   []entry `json:"results"`
		Truncated bool    `json:"truncated"`
	}
	search := func(q string) searchResp {
		resp, raw := c.do(http.MethodGet, "/api/v1/fs/search?"+q, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("search %q: %d %s", q, resp.StatusCode, raw)
		}
		return decode[searchResp](t, raw)
	}

	// A nested file is found from the root by a substring of its name.
	if got := search("q=beach"); len(got.Results) != 1 || got.Results[0].Path != "/photos/2024/beach-sunset.jpg" {
		t.Fatalf("beach = %+v, want the nested jpg", got.Results)
	}

	// A directory whose own name matches is a result too.
	if got := search("q=photos"); len(got.Results) != 1 || got.Results[0].Path != "/photos" || !got.Results[0].IsDir {
		t.Fatalf("photos = %+v, want the /photos folder", got.Results)
	}

	// A scoped search does not reach outside its root.
	if got := search("q=report&path=/photos"); len(got.Results) != 0 {
		t.Fatalf("scoped report = %+v, want nothing under /photos", got.Results)
	}

	// An empty query is a well-formed no-op, not an error.
	if got := search("q="); len(got.Results) != 0 {
		t.Fatalf("empty query = %+v, want no results", got.Results)
	}
}

func TestCopyFolderRunsAsBackgroundJob(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/src/inner"})
	if err := os.WriteFile(filepath.Join(c.root, "src", "a.txt"), []byte("aaa"), 0o644); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(c.root, "src", "inner", "b.txt"), []byte("bbbb"), 0o644); err != nil {
		t.Fatalf("seed b: %v", err)
	}

	// Copying a directory is accepted as a background job, not refused.
	resp, raw := c.do(http.MethodPost, "/api/v1/fs/copy", map[string]string{"from": "/src", "to": "/dst"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("copy folder = %d, want 202: %s", resp.StatusCode, raw)
	}
	wrap := decode[struct {
		Job struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"job"`
	}](t, raw)
	if wrap.Job.ID == 0 {
		t.Fatalf("no job id in %s", raw)
	}

	// Poll the job until it finishes.
	var status string
	for i := 0; i < 200; i++ {
		r, body := c.do(http.MethodGet, fmt.Sprintf("/api/v1/jobs/%d", wrap.Job.ID), nil)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("job get = %d: %s", r.StatusCode, body)
		}
		status = decode[struct {
			Status string `json:"status"`
		}](t, body).Status
		if status == "done" || status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status != "done" {
		t.Fatalf("job status = %q, want done", status)
	}

	// The tree is reproduced under the destination.
	top := decode[listing](t, mustList(c, "/dst"))
	if !hasEntry(top.Entries, "a.txt") || !hasEntry(top.Entries, "inner") {
		t.Fatalf("/dst = %+v, want a.txt and inner", top.Entries)
	}
	inner := decode[listing](t, mustList(c, "/dst/inner"))
	if !hasEntry(inner.Entries, "b.txt") {
		t.Fatalf("/dst/inner = %+v, want b.txt", inner.Entries)
	}
}

func mustList(c *client, path string) []byte {
	c.t.Helper()
	resp, raw := c.do(http.MethodGet, "/api/v1/fs?path="+url.QueryEscape(path), nil)
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("list %s = %d: %s", path, resp.StatusCode, raw)
	}
	return raw
}

func hasEntry(entries []entry, name string) bool {
	for _, e := range entries {
		if e.Name == name {
			return true
		}
	}
	return false
}

func TestInvitationLifecycle(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp() // creates the admin and keeps its token

	// The admin mints an invite.
	resp, raw := c.do(http.MethodPost, "/api/v1/invitations", map[string]string{"email": "bob@example.com"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create invite = %d: %s", resp.StatusCode, raw)
	}
	invite := decode[struct {
		ID    int64  `json:"id"`
		Email string `json:"email"`
		Token string `json:"token"`
	}](t, raw)
	if invite.Token == "" || invite.Email != "bob@example.com" {
		t.Fatalf("invite = %+v, want a token and the email", invite)
	}

	// The public check says it is usable.
	_, raw = c.do(http.MethodGet, "/api/v1/invitations/check?token="+url.QueryEscape(invite.Token), nil)
	if chk := decode[struct {
		Valid bool   `json:"valid"`
		Email string `json:"email"`
	}](t, raw); !chk.Valid || chk.Email != "bob@example.com" {
		t.Fatalf("check = %+v, want valid with the email", chk)
	}

	// Accepting it creates a standard, non-admin account and signs it in.
	resp, raw = c.do(http.MethodPost, "/api/v1/invitations/accept", map[string]string{
		"token": invite.Token, "username": "bob", "password": "correct horse battery",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("accept = %d: %s", resp.StatusCode, raw)
	}
	accepted := decode[struct {
		User  userJSON `json:"user"`
		Token string   `json:"token"`
	}](t, raw)
	if accepted.User.Username != "bob" || accepted.User.IsAdmin {
		t.Fatalf("accepted user = %+v, want a non-admin bob", accepted.User)
	}
	bobToken := accepted.Token

	// The same token cannot be used twice.
	_, raw = c.do(http.MethodGet, "/api/v1/invitations/check?token="+url.QueryEscape(invite.Token), nil)
	if chk := decode[struct {
		Valid bool `json:"valid"`
	}](t, raw); chk.Valid {
		t.Fatalf("used token still reports valid")
	}

	// A second invite can be listed and revoked.
	_, raw = c.do(http.MethodPost, "/api/v1/invitations", map[string]string{})
	second := decode[struct {
		ID int64 `json:"id"`
	}](t, raw)
	if list := decode[struct {
		Invitations []struct {
			ID int64 `json:"id"`
		} `json:"invitations"`
	}](t, mustGet(c, "/api/v1/invitations")); len(list.Invitations) != 1 || list.Invitations[0].ID != second.ID {
		t.Fatalf("pending list = %+v, want only the second invite", list.Invitations)
	}
	if resp, raw = c.do(http.MethodDelete, fmt.Sprintf("/api/v1/invitations/%d", second.ID), nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke = %d: %s", resp.StatusCode, raw)
	}

	// A non-admin cannot invite.
	admin := c.token
	c.token = bobToken
	resp, raw = c.do(http.MethodPost, "/api/v1/invitations", map[string]string{})
	c.token = admin
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin invite = %d, want 403: %s", resp.StatusCode, raw)
	}
}

type userJSON struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsAdmin  bool   `json:"is_admin"`
}

func mustGet(c *client, path string) []byte {
	c.t.Helper()
	resp, raw := c.do(http.MethodGet, path, nil)
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, raw)
	}
	return raw
}

func TestPermissionsEnforceSharing(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp() // admin

	if err := os.WriteFile(filepath.Join(c.root, "doc.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Bring in a non-admin through an invite.
	_, raw := c.do(http.MethodPost, "/api/v1/invitations", map[string]string{})
	inviteToken := decode[struct {
		Token string `json:"token"`
	}](t, raw).Token
	_, raw = c.do(http.MethodPost, "/api/v1/invitations/accept", map[string]string{
		"token": inviteToken, "username": "bob", "password": "correct horse battery",
	})
	bob := decode[struct {
		User  userJSON `json:"user"`
		Token string   `json:"token"`
	}](t, raw)
	admin := c.token

	// A fresh non-admin holds nothing, and admin-only endpoints refuse them.
	c.token = bob.Token
	if ps := decode[struct{ Read, Share bool }](t, mustGet(c, "/api/v1/permissions?path=/")); ps.Read || ps.Share {
		t.Fatalf("fresh non-admin perms = %+v, want none", ps)
	}
	if resp, _ := c.do(http.MethodGet, "/api/v1/users", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin GET /users = %d, want 403", resp.StatusCode)
	}
	if resp, _ := c.do(http.MethodPost, "/api/v1/access", map[string]any{
		"subject_id": bob.User.ID, "path": "/", "perms": map[string]bool{"read": true},
	}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin grant = %d, want 403", resp.StatusCode)
	}

	// Admin grants bob recursive read — but not share.
	c.token = admin
	if resp, raw := c.do(http.MethodPost, "/api/v1/access", map[string]any{
		"subject_id": bob.User.ID, "path": "/", "perms": map[string]bool{"read": true}, "recursive": true,
	}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("grant read = %d: %s", resp.StatusCode, raw)
	}

	// Bob can now read, and sharing is refused for lack of PermShare.
	c.token = bob.Token
	if ps := decode[struct{ Read, Share bool }](t, mustGet(c, "/api/v1/permissions?path=/doc.txt")); !ps.Read || ps.Share {
		t.Fatalf("after read grant, perms = %+v, want read only", ps)
	}
	if resp, raw := c.do(http.MethodPost, "/api/v1/shares", map[string]string{"path": "/doc.txt"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("share without PermShare = %d, want 403: %s", resp.StatusCode, raw)
	}

	// Admin grants share; now bob can create the link.
	c.token = admin
	c.do(http.MethodPost, "/api/v1/access", map[string]any{
		"subject_id": bob.User.ID, "path": "/", "perms": map[string]bool{"read": true, "share": true}, "recursive": true,
	})
	c.token = bob.Token
	if resp, raw := c.do(http.MethodPost, "/api/v1/shares", map[string]string{"path": "/doc.txt"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("share with PermShare = %d, want 201: %s", resp.StatusCode, raw)
	}
	c.token = admin
}

func TestUserManagement(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp() // admin "krishna"
	adminID := decode[userJSON](t, mustGet(c, "/api/v1/auth/me")).ID

	// Invite a non-admin.
	_, raw := c.do(http.MethodPost, "/api/v1/invitations", map[string]string{})
	inviteToken := decode[struct {
		Token string `json:"token"`
	}](t, raw).Token
	_, raw = c.do(http.MethodPost, "/api/v1/invitations/accept", map[string]string{
		"token": inviteToken, "username": "bob", "password": "correct horse battery",
	})
	bob := decode[struct {
		User  userJSON `json:"user"`
		Token string   `json:"token"`
	}](t, raw)

	// Give bob a rule, so deletion has ACL state to clean up.
	c.do(http.MethodPost, "/api/v1/access", map[string]any{
		"subject_id": bob.User.ID, "path": "/", "perms": map[string]bool{"read": true}, "recursive": true,
	})

	// You cannot change your own account here.
	if resp, _ := c.do(http.MethodPatch, fmt.Sprintf("/api/v1/users/%d", adminID), map[string]any{"disabled": true}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("self-disable = %d, want 400", resp.StatusCode)
	}

	// Promote bob.
	if got := decode[userJSON](t, mustPatch(c, fmt.Sprintf("/api/v1/users/%d", bob.User.ID), map[string]any{"is_admin": true})); !got.IsAdmin {
		t.Fatalf("promote: is_admin = %v, want true", got.IsAdmin)
	}

	// Disabling ends bob's sessions immediately.
	c.do(http.MethodPatch, fmt.Sprintf("/api/v1/users/%d", bob.User.ID), map[string]any{"disabled": true})
	saved := c.token
	c.token = bob.Token
	if resp, _ := c.do(http.MethodGet, "/api/v1/auth/me", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled user's session = %d, want 401", resp.StatusCode)
	}
	c.token = saved

	// Delete bob: gone from the list, and their rule is cleaned up.
	if resp, raw := c.do(http.MethodDelete, fmt.Sprintf("/api/v1/users/%d", bob.User.ID), nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", resp.StatusCode, raw)
	}
	users := decode[struct {
		Users []userJSON `json:"users"`
	}](t, mustGet(c, "/api/v1/users"))
	for _, u := range users.Users {
		if u.ID == bob.User.ID {
			t.Fatalf("bob still listed after delete")
		}
	}
	rules := decode[struct {
		Rules []struct {
			SubjectID int64 `json:"subject_id"`
		} `json:"rules"`
	}](t, mustGet(c, "/api/v1/access?path=/"))
	for _, r := range rules.Rules {
		if r.SubjectID == bob.User.ID {
			t.Fatalf("bob's ACL rule survived deletion")
		}
	}
}

func mustPatch(c *client, path string, body any) []byte {
	c.t.Helper()
	resp, raw := c.do(http.MethodPatch, path, body)
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("PATCH %s = %d: %s", path, resp.StatusCode, raw)
	}
	return raw
}

func TestListingTraversesToGrants(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/partage"})
	c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/private"})
	if err := os.WriteFile(filepath.Join(c.root, "partage", "note.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Bring in a non-admin.
	_, raw := c.do(http.MethodPost, "/api/v1/invitations", map[string]string{})
	inviteToken := decode[struct {
		Token string `json:"token"`
	}](t, raw).Token
	_, raw = c.do(http.MethodPost, "/api/v1/invitations/accept", map[string]string{
		"token": inviteToken, "username": "bob", "password": "correct horse battery",
	})
	bob := decode[struct {
		User  userJSON `json:"user"`
		Token string   `json:"token"`
	}](t, raw)
	admin := c.token

	c.token = bob.Token
	if resp, _ := c.do(http.MethodGet, "/api/v1/fs?path=/", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("fresh root = %d, want 403", resp.StatusCode)
	}
	if got := decode[listing](t, mustList(c, "/members/bob")); len(got.Entries) != 0 {
		t.Fatalf("fresh home = %+v, want empty", got.Entries)
	}

	// Grant bob read on /partage.
	c.token = admin
	c.do(http.MethodPost, "/api/v1/access", map[string]any{
		"subject_id": bob.User.ID, "path": "/partage", "perms": map[string]bool{"read": true}, "recursive": true,
	})

	c.token = bob.Token
	if resp, _ := c.do(http.MethodGet, "/api/v1/fs?path=/", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("granted root = %d, want 403", resp.StatusCode)
	}
	// He can list inside it,
	if inside := decode[listing](t, mustList(c, "/partage")); !hasEntry(inside.Entries, "note.txt") {
		t.Fatalf("/partage = %+v, want note.txt", inside.Entries)
	}
	// but a folder he has no path to is denied outright.
	if resp, _ := c.do(http.MethodGet, "/api/v1/fs?path=/private", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("/private = %d, want 403", resp.StatusCode)
	}
	c.token = admin
}

func TestRenamePreservesAccess(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/partage"})
	if err := os.WriteFile(filepath.Join(c.root, "partage", "note.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, raw := c.do(http.MethodPost, "/api/v1/invitations", map[string]string{})
	inviteToken := decode[struct {
		Token string `json:"token"`
	}](t, raw).Token
	_, raw = c.do(http.MethodPost, "/api/v1/invitations/accept", map[string]string{
		"token": inviteToken, "username": "bob", "password": "correct horse battery",
	})
	bob := decode[struct {
		User  userJSON `json:"user"`
		Token string   `json:"token"`
	}](t, raw)
	admin := c.token

	c.do(http.MethodPost, "/api/v1/access", map[string]any{
		"subject_id": bob.User.ID, "path": "/partage", "perms": map[string]bool{"read": true}, "recursive": true,
	})

	// Rename the shared folder.
	if resp, body := c.do(http.MethodPost, "/api/v1/fs/move", map[string]string{"from": "/partage", "to": "/renamed"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("rename = %d: %s", resp.StatusCode, body)
	}

	// The rule moved with it: nothing left at the old path, bob's rule at the new.
	if old := decode[struct {
		Rules []struct {
			SubjectID int64 `json:"subject_id"`
		} `json:"rules"`
	}](t, mustGet(c, "/api/v1/access?path=/partage")); len(old.Rules) != 0 {
		t.Fatalf("rules stranded at old path: %+v", old.Rules)
	}
	now := decode[struct {
		Rules []struct {
			SubjectID int64 `json:"subject_id"`
		} `json:"rules"`
	}](t, mustGet(c, "/api/v1/access?path=/renamed"))
	if len(now.Rules) != 1 || now.Rules[0].SubjectID != bob.User.ID {
		t.Fatalf("rule did not follow the rename: %+v", now.Rules)
	}

	// Bob still reaches the folder under its new name.
	c.token = bob.Token
	if inside := decode[listing](t, mustList(c, "/renamed")); !hasEntry(inside.Entries, "note.txt") {
		t.Fatalf("bob lost access after rename: %+v", inside.Entries)
	}
	c.token = admin
}

func TestProfilePasswordAndSessions(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp() // session A, kept as c.token

	// A second sign-in opens another session.
	_, raw := c.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"username": "krishna", "password": "correct horse battery",
	})
	tokenB := decode[struct {
		Token string `json:"token"`
	}](t, raw).Token

	// Both sessions are listed, exactly one marked current.
	sessions := decode[struct {
		Sessions []struct {
			ID      int64 `json:"id"`
			Current bool  `json:"current"`
		} `json:"sessions"`
	}](t, mustGet(c, "/api/v1/auth/sessions"))
	current := 0
	for _, s := range sessions.Sessions {
		if s.Current {
			current++
		}
	}
	if len(sessions.Sessions) != 2 || current != 1 {
		t.Fatalf("sessions = %+v, want 2 with one current", sessions.Sessions)
	}

	// The wrong current password is refused as a field error.
	if resp, _ := c.do(http.MethodPost, "/api/v1/auth/password", map[string]string{
		"current_password": "nope", "new_password": "new correct battery staple",
	}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong current = %d, want 400", resp.StatusCode)
	}

	// Changing it succeeds and ends every other session.
	if resp, body := c.do(http.MethodPost, "/api/v1/auth/password", map[string]string{
		"current_password": "correct horse battery", "new_password": "new correct battery staple",
	}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change = %d: %s", resp.StatusCode, body)
	}

	// The other session is dead; the current one still works.
	saved := c.token
	c.token = tokenB
	if resp, _ := c.do(http.MethodGet, "/api/v1/auth/me", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("other session after change = %d, want 401", resp.StatusCode)
	}
	c.token = saved
	if resp, _ := c.do(http.MethodGet, "/api/v1/auth/me", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("current session after change = %d, want 200", resp.StatusCode)
	}

	// The new password signs in; the old one does not.
	if resp, _ := c.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"username": "krishna", "password": "new correct battery staple",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("new password login = %d, want 200", resp.StatusCode)
	}
	if resp, _ := c.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"username": "krishna", "password": "correct horse battery",
	}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password login = %d, want 401", resp.StatusCode)
	}
}

// per-account theme tests
func TestThemePreference(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp() // admin

	// default
	me := decode[struct {
		Theme string `json:"theme"`
	}](t, mustGet(c, "/api/v1/auth/me"))
	if me.Theme != "" {
		t.Fatalf("initial theme = %q, want empty", me.Theme)
	}

	// stored theme
	if resp, body := c.do(http.MethodPut, "/api/v1/auth/theme", map[string]string{"theme": "ocean"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("set theme = %d: %s", resp.StatusCode, body)
	}
	me = decode[struct {
		Theme string `json:"theme"`
	}](t, mustGet(c, "/api/v1/auth/me"))
	if me.Theme != "ocean" {
		t.Fatalf("theme = %q, want ocean", me.Theme)
	}

	// invalid shapes
	for _, bad := range []string{"Ocean!", "../etc", "a really long theme id that runs past the limit"} {
		if resp, _ := c.do(http.MethodPut, "/api/v1/auth/theme", map[string]string{"theme": bad}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("set theme %q = %d, want 400", bad, resp.StatusCode)
		}
	}
	// explicit reset to default
	if resp, body := c.do(http.MethodPut, "/api/v1/auth/theme", map[string]string{"theme": ""}); resp.StatusCode != http.StatusOK {
		t.Fatalf("reset theme = %d: %s", resp.StatusCode, body)
	}
	me = decode[struct {
		Theme string `json:"theme"`
	}](t, mustGet(c, "/api/v1/auth/me"))
	if me.Theme != "" {
		t.Fatalf("theme after reset = %q, want empty", me.Theme)
	}
}

// invite creates a non-admin account and returns its id and session token.
func (c *client) invite(username string) (int64, string) {
	c.t.Helper()
	_, raw := c.do(http.MethodPost, "/api/v1/invitations", map[string]string{})
	token := decode[struct {
		Token string `json:"token"`
	}](t2(c), raw).Token
	_, raw = c.do(http.MethodPost, "/api/v1/invitations/accept", map[string]string{
		"token": token, "username": username, "password": "correct horse battery",
	})
	u := decode[struct {
		User  userJSON `json:"user"`
		Token string   `json:"token"`
	}](t2(c), raw)
	return u.User.ID, u.Token
}

func t2(c *client) *testing.T { return c.t }

func TestGroupsGrantAccess(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()
	admin := c.token

	bobID, bobToken := c.invite("bob")
	_, carolToken := c.invite("carol")

	c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/shared"})
	if err := os.WriteFile(filepath.Join(c.root, "shared", "note.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Create a group with bob in it.
	_, raw := c.do(http.MethodPost, "/api/v1/groups", map[string]string{"name": "team"})
	group := decode[struct {
		ID int64 `json:"id"`
	}](t, raw)
	if resp, _ := c.do(http.MethodPut, fmt.Sprintf("/api/v1/groups/%d/members/%d", group.ID, bobID), nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("add member = %d", resp.StatusCode)
	}

	// Grant the group read on /shared.
	if resp, body := c.do(http.MethodPost, "/api/v1/access", map[string]any{
		"subject_type": "group", "subject_id": group.ID, "path": "/shared",
		"perms": map[string]bool{"read": true}, "recursive": true,
	}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("grant group = %d: %s", resp.StatusCode, body)
	}

	// Bob (in the group) reaches it; carol (not) does not.
	c.token = bobToken
	if inside := decode[listing](t, mustList(c, "/shared")); !hasEntry(inside.Entries, "note.txt") {
		t.Fatalf("group member cannot see the folder: %+v", inside.Entries)
	}
	c.token = carolToken
	if resp, _ := c.do(http.MethodGet, "/api/v1/fs?path=/shared", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-member /shared = %d, want 403", resp.StatusCode)
	}

	// Deleting the group removes its rule; bob loses access.
	c.token = admin
	if resp, _ := c.do(http.MethodDelete, fmt.Sprintf("/api/v1/groups/%d", group.ID), nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete group = %d", resp.StatusCode)
	}
	if rules := decode[struct {
		Rules []struct {
			ID int64 `json:"id"`
		} `json:"rules"`
	}](t, mustGet(c, "/api/v1/access?path=/shared")); len(rules.Rules) != 0 {
		t.Fatalf("group rule survived deletion: %+v", rules.Rules)
	}
	c.token = bobToken
	if resp, _ := c.do(http.MethodGet, "/api/v1/fs?path=/shared", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ex-group-member /shared = %d, want 403", resp.StatusCode)
	}
}

func TestAuditRecordsActions(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp() // records auth.setup

	// A few auditable actions.
	c.do(http.MethodPost, "/api/v1/invitations", map[string]string{"email": "bob@example.com"})
	c.do(http.MethodPost, "/api/v1/groups", map[string]string{"name": "team"})
	c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/x"})
	if err := os.WriteFile(filepath.Join(c.root, "x", "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c.do(http.MethodDelete, "/api/v1/fs?path="+url.QueryEscape("/x/f.txt"), nil)

	list := decode[struct {
		Entries []struct {
			Actor  string `json:"actor"`
			Action string `json:"action"`
			Target string `json:"target"`
		} `json:"entries"`
		NextBefore int64 `json:"next_before"`
	}](t, mustGet(c, "/api/v1/audit"))

	seen := map[string]string{} // action -> target
	for _, e := range list.Entries {
		if e.Actor != "krishna" {
			t.Errorf("entry %q has actor %q, want krishna", e.Action, e.Actor)
		}
		seen[e.Action] = e.Target
	}
	for _, want := range []string{"auth.setup", "invitation.created", "group.created", "file.trashed"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("audit did not record %q; got %v", want, seen)
		}
	}
	if seen["file.trashed"] != "/x/f.txt" {
		t.Errorf("file.trashed target = %q, want /x/f.txt", seen["file.trashed"])
	}
}

func TestAuditIsAdminOnly(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()
	_, bobToken := c.invite("bob")

	c.token = bobToken
	if resp, _ := c.do(http.MethodGet, "/api/v1/audit", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin audit = %d, want 403", resp.StatusCode)
	}
}

func TestAPITokenAuthenticates(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp() // admin session token in c.token

	// Mint a token for this account.
	created := decode[struct {
		Token string `json:"token"`
		Info  struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Prefix string `json:"prefix"`
		} `json:"info"`
	}](t, func() []byte {
		resp, raw := c.do(http.MethodPost, "/api/v1/tokens", map[string]any{"name": "backup"})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create token = %d: %s", resp.StatusCode, raw)
		}
		return raw
	}())

	if !strings.HasPrefix(created.Token, "zefile_live_") {
		t.Fatalf("token %q lacks the zefile_live_ prefix", created.Token)
	}
	if created.Info.Prefix == "" || strings.Contains(created.Info.Prefix, created.Token[20:]) {
		t.Fatalf("prefix %q should be a short display fragment, not the full secret", created.Info.Prefix)
	}

	// Swap the session cookie for the API token: it must authenticate the same
	// account, reaching an authenticated endpoint.
	c.token = created.Token
	if resp, raw := c.do(http.MethodGet, "/api/v1/fs?path=/", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("listing with API token = %d: %s", resp.StatusCode, raw)
	}

	// The token appears in its owner's list, showing only the prefix.
	list := decode[struct {
		Tokens []struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Prefix string `json:"prefix"`
		} `json:"tokens"`
	}](t, mustGet(c, "/api/v1/tokens"))
	if len(list.Tokens) != 1 || list.Tokens[0].Name != "backup" {
		t.Fatalf("token list = %+v, want one named backup", list.Tokens)
	}
}

func TestAPITokenRevocation(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	created := decode[struct {
		Token string `json:"token"`
		Info  struct {
			ID int64 `json:"id"`
		} `json:"info"`
	}](t, func() []byte {
		_, raw := c.do(http.MethodPost, "/api/v1/tokens", map[string]any{"name": "ci"})
		return raw
	}())

	// Revoke it (still using the admin session).
	if resp, raw := c.do(http.MethodDelete, "/api/v1/tokens/"+strconv.FormatInt(created.Info.ID, 10), nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke = %d: %s", resp.StatusCode, raw)
	}

	// The revoked token no longer authenticates.
	c.token = created.Token
	if resp, _ := c.do(http.MethodGet, "/api/v1/fs?path=/", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token = %d, want 401", resp.StatusCode)
	}
}

func TestAPITokenInvalidIsRejected(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	c.token = "zefile_live_notarealtoken"
	if resp, _ := c.do(http.MethodGet, "/api/v1/fs?path=/", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bogus token = %d, want 401", resp.StatusCode)
	}
}

func TestChecksumComputesAndCaches(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	body := []byte("hello checksum")
	if err := os.WriteFile(filepath.Join(c.root, "f.bin"), body, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256(body))

	// First request: not cached, so a background job comes back.
	resp, raw := c.do(http.MethodGet, "/api/v1/fs/checksum?path="+url.QueryEscape("/f.bin"), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first checksum = %d: %s", resp.StatusCode, raw)
	}
	first := decode[struct {
		Job *struct {
			ID int64 `json:"id"`
		} `json:"job"`
	}](t, raw)
	if first.Job == nil {
		t.Fatal("expected a job on the first request")
	}

	// The worker computes it; the same call then returns the cached digest.
	var got string
	for i := 0; i < 100; i++ {
		resp, raw := c.do(http.MethodGet, "/api/v1/fs/checksum?path="+url.QueryEscape("/f.bin"), nil)
		if resp.StatusCode == http.StatusOK {
			got = decode[struct {
				Checksum struct {
					Hash      string `json:"hash"`
					Algorithm string `json:"algorithm"`
				} `json:"checksum"`
			}](t, raw).Checksum.Hash
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got != want {
		t.Fatalf("checksum = %q, want %q", got, want)
	}
}

func TestTextPreviewCapsSize(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	if err := os.WriteFile(filepath.Join(c.root, "small.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatalf("seed small: %v", err)
	}
	big := make([]byte, (2<<20)+100) // just over the 2 MiB cap
	for i := range big {
		big[i] = 'a'
	}
	if err := os.WriteFile(filepath.Join(c.root, "big.log"), big, 0o644); err != nil {
		t.Fatalf("seed big: %v", err)
	}

	type textResp struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}

	small := decode[textResp](t, mustGet(c, "/api/v1/fs/text?path="+url.QueryEscape("/small.txt")))
	if small.Content != "hello world" || small.Truncated {
		t.Fatalf("small = %+v, want the content untruncated", small)
	}

	huge := decode[textResp](t, mustGet(c, "/api/v1/fs/text?path="+url.QueryEscape("/big.log")))
	if !huge.Truncated || len(huge.Content) != 2<<20 {
		t.Fatalf("big: truncated=%v len=%d, want truncated with %d bytes", huge.Truncated, len(huge.Content), 2<<20)
	}

	// A directory is not text.
	if resp, _ := c.do(http.MethodGet, "/api/v1/fs/text?path=/", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("text of dir = %d, want 400", resp.StatusCode)
	}
}

// bareFS offers no staging, extraction, or checksum support. Stat reports
// a file and Open serves a canned body.
type bareFS struct {
	storage.FS
	body []byte
}

func (f bareFS) Stat(context.Context, storage.Path) (storage.FileInfo, error) {
	return storage.FileInfo{Size: int64(len(f.body))}, nil
}

func (f bareFS) Open(context.Context, storage.Path) (storage.File, error) {
	return cannedFile{Reader: bytes.NewReader(f.body), size: int64(len(f.body))}, nil
}

type cannedFile struct {
	*bytes.Reader
	size int64
}

func (cannedFile) Close() error { return nil }

func (f cannedFile) Stat() (fs.FileInfo, error) { return cannedInfo{size: f.size}, nil }

type cannedInfo struct{ size int64 }

func (cannedInfo) Name() string       { return "f.txt" }
func (i cannedInfo) Size() int64      { return i.size }
func (cannedInfo) Mode() os.FileMode  { return 0 }
func (cannedInfo) ModTime() time.Time { return time.Time{} }
func (cannedInfo) IsDir() bool        { return false }
func (cannedInfo) Sys() any           { return nil }

// memStore is an in-memory ThumbStore for handler tests.
type memStore struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func (m *memStore) ReadThumb(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[key]
	if !ok {
		return nil, storage.ErrNotExist
	}
	return b, nil
}

func (m *memStore) WriteThumb(_ context.Context, key string, thumb []byte) error {
	m.mu.Lock()
	m.blobs[key] = thumb
	m.mu.Unlock()
	return nil
}

// objectClient builds a server over bareFS in object-storage mode. It
// returns the thumb store for pre-seeding stored thumbnails.
func objectClient(t *testing.T, previewMaxBytes int64, withJobs ...bool) (*httptest.Server, string, *memStore) {
	t.Helper()

	database, err := db.Open(t.Context(), db.Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	service := auth.New(database, auth.WithParams(auth.Params{
		Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	}))
	fs := bareFS{body: []byte("hello world")}
	store := &memStore{blobs: make(map[string][]byte)}
	opts := api.Options{
		FS:              fs,
		Auth:            service,
		ACL:             acl.New(database),
		Checksums:       checksum.New(database, fs),
		PreviewMaxBytes: previewMaxBytes,
		ObjectStorage:   true,
		Thumbs:          thumb.New(fs, store, jobsStub{}, 512<<20, nil),
		SecureCookies:   false,
	}
	if len(withJobs) > 0 && withJobs[0] {
		opts.Jobs = job.New(database)
	}
	srv := httptest.NewServer(api.New(opts).Handler())
	t.Cleanup(srv.Close)

	token, err := service.IssueSetupToken(t.Context())
	if err != nil {
		t.Fatalf("IssueSetupToken: %v", err)
	}
	payload, _ := json.Marshal(map[string]string{
		"token": token, "username": "krishna", "password": "correct horse battery",
	})
	resp, err := srv.Client().Post(srv.URL+"/api/v1/setup", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer resp.Body.Close()
	var session struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		t.Fatalf("decode setup: %v", err)
	}
	return srv, session.Token, store
}

// jobsStub drops background work: handler tests drive Generate directly.
type jobsStub struct{}

func (jobsStub) Enqueue(context.Context, job.Type, any) (job.Job, error) {
	return job.Job{}, nil
}

func objectGet(t *testing.T, srv *httptest.Server, token, path string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func objectPost(t *testing.T, srv *httptest.Server, token, path string, body any) int {
	t.Helper()
	encoded, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, bytes.NewReader(encoded))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}

func TestObjectStorageRefusesByteJobs(t *testing.T) {
	t.Parallel()

	srv, token, _ := objectClient(t, 0)

	if code := objectPost(t, srv, token, "/api/v1/fs/extract", map[string]string{"archive": "/a.zip"}); code != http.StatusNotImplemented {
		t.Errorf("extract = %d, want 501", code)
	}
	if code := objectPost(t, srv, token, "/api/v1/fs/fetch", map[string]string{"url": "https://example.com/f"}); code != http.StatusNotImplemented {
		t.Errorf("fetch = %d, want 501", code)
	}

	if code, _ := objectGet(t, srv, token, "/api/v1/fs/checksum?path=/f.bin"); code != http.StatusServiceUnavailable {
		t.Errorf("checksum without a worker = %d, want 503", code)
	}
}

func TestObjectStorageChecksumFallsBackToJob(t *testing.T) {
	t.Parallel()

	srv, token, _ := objectClient(t, 0, true)
	code, raw := objectGet(t, srv, token, "/api/v1/fs/checksum?path=/f.bin")
	if code != http.StatusAccepted {
		t.Fatalf("checksum = %d: %s, want 202 with a job to poll", code, raw)
	}
	var out struct {
		Checksum *checksum.Checksum `json:"checksum"`
		Job      *struct{}          `json:"job"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Job == nil || out.Checksum != nil {
		t.Errorf("response = %s, want a job and no checksum yet", raw)
	}
}

func TestPreviewMaxBytesIsConfigurable(t *testing.T) {
	t.Parallel()

	srv, token, _ := objectClient(t, 5)

	code, raw := objectGet(t, srv, token, "/api/v1/fs/text?path=/f.txt")
	if code != http.StatusOK {
		t.Fatalf("text = %d: %s", code, raw)
	}
	var out struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Content != "hello" || !out.Truncated {
		t.Errorf("text = %+v, want the first 5 bytes truncated", out)
	}
}

func TestStoredThumbnailIsServed(t *testing.T) {
	t.Parallel()

	srv, token, store := objectClient(t, 0)

	jpeg := []byte{0xff, 0xd8, 0xff, 0xd9}
	key := thumb.Key("/f.png", int64(len("hello world")), time.Time{})
	store.mu.Lock()
	store.blobs[key] = jpeg
	store.mu.Unlock()

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet,
		srv.URL+"/api/v1/fs/thumb?path="+url.QueryEscape("/f.png"), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET thumb: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("thumb = %d: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	if !bytes.Equal(raw, jpeg) {
		t.Errorf("thumb = %v, want the stored bytes", raw)
	}
}

func TestSpaceReportsUsageAndQuota(t *testing.T) {
	c := newClient(t)
	c.setUp() // admin krishna

	if err := os.WriteFile(filepath.Join(c.root, "ledger.bin"), make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	users := decode[struct {
		Users []struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"users"`
	}](t, mustGet(c, "/api/v1/users"))
	var adminID int64
	for _, u := range users.Users {
		if u.Username == "krishna" {
			adminID = u.ID
		}
	}
	if adminID == 0 {
		t.Fatal("krishna missing from user list")
	}
	p := storage.MustParsePath("/ledger.bin")
	if err := c.aclEngi.SetOwner(t.Context(), p, adminID); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}
	c.quota.Invalidate()

	space := decode[struct {
		Used          int64 `json:"used"`
		Quota         int64 `json:"quota"`
		InstanceUsed  int64 `json:"instance_used"`
		InstanceTotal int64 `json:"instance_total"`
	}](t, mustGet(c, "/api/v1/fs/space"))
	if space.Used != 1024 {
		t.Errorf("used = %d, want 1024", space.Used)
	}
	if space.Quota != 0 {
		t.Errorf("quota = %d, want 0 (unlimited by default)", space.Quota)
	}
	if space.InstanceUsed != 1024 {
		t.Errorf("instance_used = %d, want 1024", space.InstanceUsed)
	}

	resp, raw := c.do(http.MethodPost, "/api/v1/invitations", map[string]string{"email": "bob@example.com"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create invite = %d: %s", resp.StatusCode, raw)
	}
	invite := decode[struct {
		Token string `json:"token"`
	}](t, raw)
	resp, raw = c.do(http.MethodPost, "/api/v1/invitations/accept", map[string]string{
		"token": invite.Token, "username": "bob", "password": "correct horse battery",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("accept = %d: %s", resp.StatusCode, raw)
	}
	bob := decode[struct {
		User userJSON `json:"user"`
	}](t, raw).User

	resp, raw = c.do(http.MethodPatch, fmt.Sprintf("/api/v1/users/%d", bob.ID), map[string]int64{"quota_bytes": 2048})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set quota = %d: %s", resp.StatusCode, raw)
	}
	if got := decode[struct {
		QuotaBytes int64 `json:"quota_bytes"`
	}](t, raw); got.QuotaBytes != 2048 {
		t.Errorf("quota_bytes = %d, want 2048", got.QuotaBytes)
	}

	resp, _ = c.do(http.MethodPatch, fmt.Sprintf("/api/v1/users/%d", bob.ID), map[string]int64{"quota_bytes": -1})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative quota = %d, want 400", resp.StatusCode)
	}

	listed := decode[struct {
		Users []struct {
			Username   string `json:"username"`
			QuotaBytes int64  `json:"quota_bytes"`
			UsedBytes  int64  `json:"used_bytes"`
		} `json:"users"`
	}](t, mustGet(c, "/api/v1/users"))
	seen := map[string][2]int64{}
	for _, u := range listed.Users {
		seen[u.Username] = [2]int64{u.QuotaBytes, u.UsedBytes}
	}
	if seen["bob"] != [2]int64{2048, 0} {
		t.Errorf("bob = %v, want {2048 0}", seen["bob"])
	}
	if seen["krishna"] != [2]int64{0, 1024} {
		t.Errorf("krishna = %v, want {0 1024}", seen["krishna"])
	}
}

func TestMemberHomeUpload(t *testing.T) {
	c := newClient(t)
	c.setUp() // admin krishna

	if resp, raw := c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/shared"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mkdir /shared = %d: %s", resp.StatusCode, raw)
	}

	bobToken, _ := inviteBob(t, c)

	c.token = bobToken
	if resp, _ := c.do(http.MethodGet, "/api/v1/fs", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob root = %d, want 403", resp.StatusCode)
	}
	if got := decode[listing](t, mustGet(c, "/api/v1/fs?path=/members/bob")); len(got.Entries) != 0 {
		t.Fatalf("bob home = %+v, want empty", got.Entries)
	}

	body := []byte("bob was here")
	loc := tusCreate(t, c, "/members/bob/note.txt", int64(len(body)))
	tusWrite(t, c, loc, 0, body, true)

	listed := decode[listing](t, mustGet(c, "/api/v1/fs?path=/members/bob"))
	if len(listed.Entries) != 1 || listed.Entries[0].Size != int64(len(body)) {
		t.Fatalf("/members/bob = %+v, want the uploaded file", listed.Entries)
	}

	if resp, raw := tusCreateRaw(c, "/shared/evil.txt", 3); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("upload outside home = %d: %s, want 403", resp.StatusCode, raw)
	}
}

func TestLoginBackfillsHome(t *testing.T) {
	c := newClient(t)
	c.setUp()
	_, bobID := inviteBob(t, c)
	if err := c.aclEngi.RevokeAllForSubject(t.Context(), acl.SubjectUser, bobID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(c.root, "members", "bob")); err != nil {
		t.Fatalf("remove home: %v", err)
	}

	resp, raw := c.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"username": "bob", "password": "correct horse battery",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d: %s", resp.StatusCode, raw)
	}
	c.token = decode[struct {
		Token string `json:"token"`
	}](t, raw).Token

	if resp, _ := c.do(http.MethodGet, "/api/v1/fs", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob root after relogin = %d, want 403", resp.StatusCode)
	}
	got := decode[listing](t, mustGet(c, "/api/v1/fs?path=/members/bob"))
	if len(got.Entries) != 0 {
		t.Fatalf("bob home after relogin = %+v, want empty", got.Entries)
	}
	loc := tusCreate(t, c, "/members/bob/again.txt", 1)
	tusWrite(t, c, loc, 0, []byte("x"), true)
}

func inviteBob(t *testing.T, c *client) (string, int64) {
	t.Helper()
	admin := c.token
	resp, raw := c.do(http.MethodPost, "/api/v1/invitations", map[string]string{"email": "bob@example.com"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create invite = %d: %s", resp.StatusCode, raw)
	}
	invite := decode[struct {
		Token string `json:"token"`
	}](t, raw)
	resp, raw = c.do(http.MethodPost, "/api/v1/invitations/accept", map[string]string{
		"token": invite.Token, "username": "bob", "password": "correct horse battery",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("accept = %d: %s", resp.StatusCode, raw)
	}
	accepted := decode[struct {
		User  userJSON `json:"user"`
		Token string   `json:"token"`
	}](t, raw)
	c.token = admin
	if accepted.User.Username != "bob" {
		t.Fatalf("accepted user = %+v, want bob", accepted.User)
	}
	return accepted.Token, accepted.User.ID
}

func tusCreate(t *testing.T, c *client, path string, size int64) string {
	t.Helper()
	resp, _ := tusCreateRaw(c, path, size)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("tus create %s = %d", path, resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatalf("tus create %s returned no Location", path)
	}
	resp.Body.Close()
	return loc
}

func tusCreateRaw(c *client, path string, size int64) (*http.Response, []byte) {
	c.t.Helper()
	meta := "path " + base64.StdEncoding.EncodeToString([]byte(path))
	req, err := http.NewRequestWithContext(c.t.Context(), http.MethodPost, c.server.URL+"/api/v1/uploads", nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.FormatInt(size, 10))
	req.Header.Set("Upload-Metadata", meta)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp, raw
}

func tusWrite(t *testing.T, c *client, loc string, offset int64, body []byte, _ bool) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, c.server.URL+loc, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tus write = %d: %s", resp.StatusCode, raw)
	}
	var done struct {
		Complete bool `json:"complete"`
	}
	if err := json.Unmarshal(raw, &done); err != nil || !done.Complete {
		t.Fatalf("tus write = %+v, %v; want complete", done, err)
	}
}

func TestSharedWithMe(t *testing.T) {
	c := newClient(t)
	c.setUp() // admin krishna

	if resp, raw := c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/partage"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mkdir /partage = %d: %s", resp.StatusCode, raw)
	}
	if resp, raw := c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/private"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mkdir /private = %d: %s", resp.StatusCode, raw)
	}

	bobToken, bobID := inviteBob(t, c)

	grant := func(path string) {
		t.Helper()
		resp, raw := c.do(http.MethodPost, "/api/v1/access", map[string]any{
			"subject_id": bobID, "path": path, "perms": map[string]bool{"read": true}, "recursive": true,
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("grant %s = %d: %s", path, resp.StatusCode, raw)
		}
	}
	grant("/partage")
	grant("/ghost")

	admin := c.token
	c.token = bobToken
	got := decode[listing](t, mustGet(c, "/api/v1/shared-with-me"))
	c.token = admin
	if len(got.Entries) != 1 || got.Entries[0].Path != "/partage" || !got.Entries[0].IsDir {
		t.Fatalf("shared-with-me = %+v, want only /partage", got.Entries)
	}

	c.token = bobToken
	if resp, _ := c.do(http.MethodGet, "/api/v1/fs", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob root = %d, want 403", resp.StatusCode)
	}
	c.token = admin

	if got := decode[listing](t, mustGet(c, "/api/v1/shared-with-me")); len(got.Entries) != 0 {
		t.Fatalf("admin shared-with-me = %+v, want empty", got.Entries)
	}
}

func TestTrashIsPrivate(t *testing.T) {
	c := newClient(t)
	c.setUp() // admin krishna
	bobToken, _ := inviteBob(t, c)

	seed := func(dir, name string) {
		t.Helper()
		if resp, raw := c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": dir}); resp.StatusCode != http.StatusCreated {
			t.Fatalf("mkdir %s = %d: %s", dir, resp.StatusCode, raw)
		}
		if err := os.WriteFile(filepath.Join(c.root, strings.TrimPrefix(dir, "/"), name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	admin := c.token

	c.token = bobToken
	seed("/members/bob/docs", "note.txt")
	if resp, raw := c.do(http.MethodDelete, "/api/v1/fs?path=/members/bob/docs/note.txt", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("bob delete = %d: %s", resp.StatusCode, raw)
	}
	c.token = admin
	seed("/secret", "plan.txt")
	if resp, raw := c.do(http.MethodDelete, "/api/v1/fs?path=/secret/plan.txt", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("admin delete = %d: %s", resp.StatusCode, raw)
	}

	c.token = bobToken
	mine := decode[trashList](t, mustGet(c, "/api/v1/trash"))
	c.token = admin
	all := decode[trashList](t, mustGet(c, "/api/v1/trash"))
	if len(mine.Items) != 1 || mine.Items[0].OriginalPath != "/members/bob/docs/note.txt" {
		t.Fatalf("bob trash = %+v, want only his note", mine.Items)
	}
	if len(all.Items) != 2 {
		t.Fatalf("admin trash = %+v, want both", all.Items)
	}
	var foreign int64
	for _, it := range all.Items {
		if it.OriginalPath == "/secret/plan.txt" {
			foreign = it.ID
		}
	}

	c.token = bobToken
	if resp, raw := c.do(http.MethodPost, fmt.Sprintf("/api/v1/trash/%d/restore", foreign), nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("restore foreign = %d: %s, want 404", resp.StatusCode, raw)
	}
	if resp, raw := c.do(http.MethodDelete, fmt.Sprintf("/api/v1/trash/%d", foreign), nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("purge foreign = %d: %s, want 404", resp.StatusCode, raw)
	}

	if resp, raw := c.do(http.MethodDelete, "/api/v1/trash", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("empty = %d: %s", resp.StatusCode, raw)
	}
	if items := decode[trashList](t, mustGet(c, "/api/v1/trash")).Items; len(items) != 0 {
		t.Fatalf("bob trash after empty = %+v, want none", items)
	}
	c.token = admin
	if items := decode[trashList](t, mustGet(c, "/api/v1/trash")).Items; len(items) != 1 || items[0].ID != foreign {
		t.Fatalf("admin trash after bob empty = %+v, want the plan", items)
	}

	if resp, raw := c.do(http.MethodPost, fmt.Sprintf("/api/v1/trash/%d/restore", foreign), nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("admin restore = %d: %s", resp.StatusCode, raw)
	}
}

func TestJobsArePrivate(t *testing.T) {
	c := newClient(t)
	c.setUp() // admin krishna
	bobToken, _ := inviteBob(t, c)

	if resp, raw := c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/dossier"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mkdir = %d: %s", resp.StatusCode, raw)
	}
	resp, raw := c.do(http.MethodPost, "/api/v1/fs/copy", map[string]string{"from": "/dossier", "to": "/copie"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("copy dir = %d: %s, want 202", resp.StatusCode, raw)
	}
	jobID := decode[struct {
		Job struct {
			ID int64 `json:"id"`
		} `json:"job"`
	}](t, raw).Job.ID

	if jobs := decode[jobList](t, mustGet(c, "/api/v1/jobs")).Jobs; len(jobs) != 1 || jobs[0].ID != jobID {
		t.Fatalf("admin jobs = %+v, want the copy", jobs)
	}

	admin := c.token
	c.token = bobToken
	if jobs := decode[jobList](t, mustGet(c, "/api/v1/jobs")).Jobs; len(jobs) != 0 {
		t.Fatalf("bob jobs = %+v, want none", jobs)
	}
	if resp, raw := c.do(http.MethodGet, fmt.Sprintf("/api/v1/jobs/%d", jobID), nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob get foreign job = %d: %s, want 404", resp.StatusCode, raw)
	}
	if resp, raw := c.do(http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/cancel", jobID), nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob cancel foreign job = %d: %s, want 404", resp.StatusCode, raw)
	}
	c.token = admin
}

func TestMembersDirIsSystemManaged(t *testing.T) {
	c := newClient(t)
	c.setUp() // admin krishna
	bobToken, _ := inviteBob(t, c)
	admin := c.token

	refused := func(method, path string, body any) {
		t.Helper()
		if resp, raw := c.do(method, path, body); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s = %d: %s, want 403", method, path, resp.StatusCode, raw)
		}
	}

	refused(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/members"})
	refused(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/members/eve"})
	refused(http.MethodDelete, "/api/v1/fs?path=/members", nil)
	refused(http.MethodDelete, "/api/v1/fs?path=/members/bob", nil)
	refused(http.MethodPost, "/api/v1/fs/move", map[string]string{"from": "/members/bob", "to": "/members/robert"})
	refused(http.MethodPost, "/api/v1/fs/move", map[string]string{"from": "/members/bob", "to": "/elsewhere"})
	if resp, raw := tusCreateRaw(c, "/members/drop.txt", 3); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("upload to /members = %d: %s, want 403", resp.StatusCode, raw)
	}

	c.token = bobToken
	refused(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/members/eve"})
	refused(http.MethodDelete, "/api/v1/fs?path=/members/bob", nil)
	c.token = admin

	if resp, raw := c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/members/bob/docs"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mkdir in home = %d: %s", resp.StatusCode, raw)
	}
	if resp, raw := c.do(http.MethodPost, "/api/v1/fs/move", map[string]string{"from": "/members/bob/docs", "to": "/members/bob/papers"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("move in home = %d: %s", resp.StatusCode, raw)
	}
	if resp, raw := c.do(http.MethodDelete, "/api/v1/fs?path=/members/bob/papers", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete in home = %d: %s", resp.StatusCode, raw)
	}

	c.token = bobToken
	if resp, _ := c.do(http.MethodGet, "/api/v1/fs?path=/members/bob", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("bob home = %d, want 200", resp.StatusCode)
	}
	c.token = admin
}
