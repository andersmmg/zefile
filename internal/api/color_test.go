package api_test

import (
	"net/http"
	"testing"

	"github.com/krishna2206/zefile/internal/color"
)

func entryNamed(t *testing.T, entries []entry, name string) entry {
	t.Helper()
	for _, e := range entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry %q in %+v", name, entries)
	return entry{}
}

func setColor(c *client, path, id string) {
	c.t.Helper()
	resp, raw := c.do(http.MethodPut, "/api/v1/fs/color", map[string]string{"path": path, "color": id})
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("set colour %s = %d: %s", path, resp.StatusCode, raw)
	}
}

// Colours are validated against the palette, follow renames and the trash, and
// are never visible to another account.
func TestEntryColors(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	c.setUp()

	if _, raw := c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/projets/alpha"}); raw == nil {
		t.Fatal("mkdir returned nothing")
	}

	resp, raw := c.do(http.MethodPut, "/api/v1/fs/color", map[string]string{"path": "/projets", "color": "chartreuse"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown colour = %d: %s", resp.StatusCode, raw)
	}

	setColor(c, "/projets", "teal")
	if got := entryNamed(t, decode[listing](t, mustList(c, "/")).Entries, "projets"); got.Color != "teal" {
		t.Fatalf("listed colour = %q, want teal", got.Color)
	}
	_, raw = c.do(http.MethodGet, "/api/v1/fs/search?q=projets", nil)
	results := decode[struct {
		Results []entry `json:"results"`
	}](t, raw).Results
	if len(results) == 0 || results[0].Color != "teal" {
		t.Fatalf("search colour = %+v, want teal", results)
	}

	if resp, raw := c.do(http.MethodPost, "/api/v1/fs/move", map[string]string{"from": "/projets", "to": "/travaux"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("move = %d: %s", resp.StatusCode, raw)
	}
	if got := entryNamed(t, decode[listing](t, mustList(c, "/")).Entries, "travaux"); got.Color != "teal" {
		t.Fatalf("colour after rename = %q, want teal", got.Color)
	}
	// The nested label is read a directory at a time, so its stored parent has
	// to follow the rename too.
	setColor(c, "/travaux/alpha", "purple")
	if got := entryNamed(t, decode[listing](t, mustList(c, "/travaux")).Entries, "alpha"); got.Color != "purple" {
		t.Fatalf("nested colour = %q, want purple", got.Color)
	}

	// Clearing is how "no colour" is expressed, so it must be safe to repeat.
	for range 2 {
		if resp, raw := c.do(http.MethodPut, "/api/v1/fs/color", map[string]string{"path": "/travaux"}); resp.StatusCode != http.StatusOK {
			t.Fatalf("clear colour = %d: %s", resp.StatusCode, raw)
		}
	}
	if got := entryNamed(t, decode[listing](t, mustList(c, "/")).Entries, "travaux"); got.Color != "" {
		t.Fatalf("colour after clear = %q, want none", got.Color)
	}

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
		"subject_id": bob.User.ID, "path": "/travaux", "perms": map[string]bool{"read": true}, "recursive": true,
	})

	setColor(c, "/travaux", "pink")
	c.token = bob.Token
	setColor(c, "/travaux", "green")
	if got := entryNamed(t, decode[listing](t, mustList(c, "/travaux")).Entries, "alpha"); got.Color != "" {
		t.Fatalf("another user's colour = %q, want none", got.Color)
	}
	c.token = admin
	if got := entryNamed(t, decode[listing](t, mustList(c, "/")).Entries, "travaux"); got.Color != "pink" {
		t.Fatalf("own colour after another set one = %q, want pink", got.Color)
	}

	if resp, raw := c.do(http.MethodDelete, "/api/v1/fs?path=/travaux", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", resp.StatusCode, raw)
	}
	if _, raw := c.do(http.MethodPost, "/api/v1/fs/dirs", map[string]string{"path": "/travaux"}); raw == nil {
		t.Fatal("re-create returned nothing")
	}
	if got := entryNamed(t, decode[listing](t, mustList(c, "/")).Entries, "travaux"); got.Color != "" {
		t.Fatalf("colour inherited by a reused name = %q, want none", got.Color)
	}
	setColor(c, "/travaux", "blue")
	if got := entryNamed(t, decode[listing](t, mustList(c, "/")).Entries, "travaux"); got.Color != "blue" {
		t.Fatalf("colour after re-labelling = %q, want blue", got.Color)
	}
}

func TestPaletteIsValidated(t *testing.T) {
	t.Parallel()

	if !color.Valid("teal") || color.Valid("chartreuse") || color.Valid("") {
		t.Fatalf("palette validation disagrees with Presets: %v", color.Presets)
	}
}
