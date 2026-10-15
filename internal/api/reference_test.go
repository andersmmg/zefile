package api_test

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setReference(c *client, path, target string) {
	c.t.Helper()
	resp, raw := c.do(http.MethodPut, "/api/v1/fs/reference", map[string]string{"path": path, "target": target})
	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("set reference %s -> %s = %d: %s", path, target, resp.StatusCode, raw)
	}
}

func clearReference(c *client, path string) {
	c.t.Helper()
	resp, raw := c.do(http.MethodDelete, "/api/v1/fs/reference?path="+url.QueryEscape(path), nil)
	if resp.StatusCode != http.StatusNoContent {
		c.t.Fatalf("clear reference %s = %d: %s", path, resp.StatusCode, raw)
	}
}

func TestReferences(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	if err := os.WriteFile(filepath.Join(c.root, "target.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	// Point a reference at the file; the listing reports the target path.
	setReference(c, "/link", "/target.txt")
	if got := entryNamed(t, decode[listing](t, mustList(c, "/")).Entries, "link"); got.Ref != "/target.txt" {
		t.Fatalf("listed reference = %q, want /target.txt", got.Ref)
	}

	// The folder total counts the reference as a file.
	_, raw := c.do(http.MethodGet, "/api/v1/fs/count?path=/", nil)
	count := decode[struct {
		Files int64 `json:"files"`
		Dirs  int64 `json:"dirs"`
	}](t, raw)
	if count.Files != 2 || count.Dirs != 0 {
		t.Fatalf("count with a reference = %d files, %d dirs, want 2 files", count.Files, count.Dirs)
	}

	// A reference to a path that does not exist is refused.
	if resp, raw := c.do(http.MethodPut, "/api/v1/fs/reference", map[string]string{"path": "/link2", "target": "/nope.txt"}); resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("reference to missing target = %d, want 4xx: %s", resp.StatusCode, raw)
	}

	// Renaming the target follows the reference.
	if resp, raw := c.do(http.MethodPost, "/api/v1/fs/move", map[string]string{"from": "/target.txt", "to": "/renamed.txt"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("move = %d: %s", resp.StatusCode, raw)
	}
	if got := entryNamed(t, decode[listing](t, mustList(c, "/")).Entries, "link"); got.Ref != "/renamed.txt" {
		t.Fatalf("reference after target rename = %q, want /renamed.txt", got.Ref)
	}

	// Moving the reference itself relocates it, keeping its target.
	if resp, raw := c.do(http.MethodPost, "/api/v1/fs/move", map[string]string{"from": "/link", "to": "/link2"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("move reference = %d: %s", resp.StatusCode, raw)
	}
	if got := entryNamed(t, decode[listing](t, mustList(c, "/")).Entries, "link2"); got.Ref != "/renamed.txt" {
		t.Fatalf("reference after its own move = %q, want /renamed.txt", got.Ref)
	}

	// Clearing drops the reference, leaving the target's own entry intact.
	clearReference(c, "/link2")
	if _, raw := c.do(http.MethodGet, "/api/v1/fs/stat?path=/link2", nil); raw == nil {
		t.Fatal("clearing a reference left it behind")
	}
	if resp, raw := c.do(http.MethodGet, "/api/v1/fs/stat?path=/renamed.txt", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("clearing the reference disturbed its target: %d %s", resp.StatusCode, raw)
	}

	// The total drops back to the folder's real files.
	_, raw = c.do(http.MethodGet, "/api/v1/fs/count?path=/", nil)
	if count := decode[struct {
		Files int64 `json:"files"`
	}](t, raw); count.Files != 1 {
		t.Fatalf("count after clearing = %d files, want 1", count.Files)
	}

	// References are per user: another account sees the same path with no pointer.
	_, raw = c.do(http.MethodPost, "/api/v1/invitations", map[string]string{})
	invite := decode[struct {
		Token string `json:"token"`
	}](t, raw).Token
	admin := c.token
	_, raw = c.do(http.MethodPost, "/api/v1/invitations/accept", map[string]string{
		"token": invite, "username": "rene", "password": "correct horse battery",
	})
	bob := decode[struct {
		User  userJSON `json:"user"`
		Token string   `json:"token"`
	}](t, raw)
	c.do(http.MethodPost, "/api/v1/access", map[string]any{
		"subject_id": bob.User.ID, "path": "/", "perms": map[string]bool{"read": true}, "recursive": true,
	})

	bobHome := "/members/rene"
	// The pointer is bob's own, created in his home: a reference carries no
	// access for anyone else, so an admin cannot install one on his behalf and
	// expect him to see it.
	c.token = bob.Token
	setReference(c, bobHome+"/mine", "/renamed.txt")
	if got := entryNamed(t, decode[listing](t, mustList(c, bobHome)).Entries, "mine"); got.Ref != "/renamed.txt" {
		t.Fatalf("owner's reference = %q, want /renamed.txt", got.Ref)
	}
	// The admin listing the same folder sees no trace of bob's pointer.
	c.token = admin
	if hasEntry(decode[listing](t, mustList(c, bobHome)).Entries, "mine") {
		t.Fatal("a reference leaked into another account's listing")
	}

	// A reference cannot be aimed at something the caller cannot read.
	c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/hidden"})
	// (The admin can read everything, so this is exercised for bob below!)

	// Dropping a target trashes it.
	if resp, raw := c.do(http.MethodDelete, "/api/v1/fs?path=/renamed.txt", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete target = %d: %s", resp.StatusCode, raw)
	}
	c.token = bob.Token
	if got := entryNamed(t, decode[listing](t, mustList(c, bobHome)).Entries, "mine"); got.Ref != "" {
		t.Fatalf("dangling reference = %q, want none", got.Ref)
	}
}

// Reading through a reference acts on the target
func TestReferenceReadsFollowTarget(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	if err := os.WriteFile(filepath.Join(c.root, "doc.txt"), []byte("hello ref"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	if err := os.WriteFile(filepath.Join(c.root, "photo.png"), img.Bytes(), 0o644); err != nil {
		t.Fatalf("seed image: %v", err)
	}
	setReference(c, "/note", "/doc.txt")
	setReference(c, "/pic", "/photo.png")

	// The text preview shows the original file's content.
	_, raw := c.do(http.MethodGet, "/api/v1/fs/text?path=/note", nil)
	got := decode[struct {
		Content string `json:"content"`
	}](t, raw)
	if got.Content != "hello ref" {
		t.Fatalf("text through reference = %q, want %q", got.Content, "hello ref")
	}

	// The details panel describes the target's size but keeps the reference's
	// own name.
	_, raw = c.do(http.MethodGet, "/api/v1/fs/info?path=/note", nil)
	info := decode[struct {
		Path string `json:"path"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	}](t, raw)
	if info.Size != int64(len("hello ref")) || info.Name != "note" || info.Path != "/note" {
		t.Fatalf("info through reference = %+v, want target size with the reference's own name", info)
	}

	// The preview link is minted for the target, so the URL names the original.
	_, raw = c.do(http.MethodGet, "/api/v1/fs/link?path=/note", nil)
	link := decode[struct {
		URL string `json:"url"`
	}](t, raw)
	if !strings.HasSuffix(link.URL, "/doc.txt") {
		t.Fatalf("link through reference = %q, want the target's name", link.URL)
	}

	// The thumbnail is the target's thumbnail, so the URL names the original.
	if resp, raw := c.do(http.MethodGet, "/api/v1/fs/thumb?path=/pic&s=64", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("thumb through reference = %d, want 200: %s", resp.StatusCode, raw)
	}

	// Once the target is gone the reference leads nowhere.
	if resp, raw := c.do(http.MethodDelete, "/api/v1/fs?path=/doc.txt", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete target = %d: %s", resp.StatusCode, raw)
	}
	for _, ep := range []string{
		"/api/v1/fs/text?path=/note",
		"/api/v1/fs/info?path=/note",
		"/api/v1/fs/link?path=/note",
	} {
		if resp, raw := c.do(http.MethodGet, ep, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("dangling reference %s = %d, want 404: %s", ep, resp.StatusCode, raw)
		}
	}
}

// Entering a linked folder directs to the original folder.
func TestLinkedFolderListsTarget(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	if err := os.MkdirAll(filepath.Join(c.root, "real"), 0o755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(c.root, "real", "inside.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	setReference(c, "/folder-link", "/real")

	entries := decode[listing](t, mustList(c, "/folder-link")).Entries
	if !hasEntry(entries, "inside.txt") {
		t.Fatalf("listing through a linked folder = %+v, want inside.txt", entries)
	}

	_, raw := c.do(http.MethodGet, "/api/v1/fs/count?path=/folder-link", nil)
	count := decode[struct {
		Files int64 `json:"files"`
		Dirs  int64 `json:"dirs"`
	}](t, raw)
	if count.Files != 1 || count.Dirs != 0 {
		t.Fatalf("count through a linked folder = %d files, %d dirs, want 1 file", count.Files, count.Dirs)
	}

	_, raw = c.do(http.MethodGet, "/api/v1/fs/search?q=inside&path=/folder-link", nil)
	found := decode[struct {
		Results []entry `json:"results"`
	}](t, raw)
	if !hasEntry(found.Results, "inside.txt") {
		t.Fatalf("search under a linked folder = %+v, want inside.txt", found.Results)
	}
}

func TestReferenceEnforcesTargetRead(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/private"})
	if err := os.WriteFile(filepath.Join(c.root, "private", "secret.txt"), []byte("shh"), 0o644); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	_, raw := c.do(http.MethodPost, "/api/v1/invitations", map[string]string{})
	invite := decode[struct {
		Token string `json:"token"`
	}](t, raw).Token
	_, raw = c.do(http.MethodPost, "/api/v1/invitations/accept", map[string]string{
		"token": invite, "username": "rene", "password": "correct horse battery",
	})
	bob := decode[struct {
		User  userJSON `json:"user"`
		Token string   `json:"token"`
	}](t, raw)

	c.token = bob.Token
	if resp, raw := c.do(http.MethodPut, "/api/v1/fs/reference", map[string]string{
		"path": "/bob-link", "target": "/private/secret.txt",
	}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("reference to unreadable target = %d, want 403: %s", resp.StatusCode, raw)
	}
}
