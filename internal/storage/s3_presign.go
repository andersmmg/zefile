package storage

import (
	"sync"
	"time"
)

// ensures browsers revalidate cached previews by attaching
// "private, max-age=0, must-revalidate" so a 304 costs one tiny round trip
// instead of a full download, keeping stale bytes from old files off-screen.
const presignCacheControl = "private, max-age=0, must-revalidate"

// keeps a reused capability URL comfortably inside its
// validity window: a browser that starts fetching the moment we hand the URL
// out still has this long before the store would reject it.
const presignReuseMargin = time.Minute

// bounds the reuse table. Entries expire together with
// their TTL windows, but the cap makes sure the table cant grow without
// limit on a long-lived instance.
const presignMaxEntries = 1024

type presignEntry struct {
	url   string
	until time.Time
}

// reuses presigned URLs within their validity window so repeated previews
// hit the browser cache.
type presignCache struct {
	mu      sync.Mutex
	entries map[string]presignEntry
}

// get returns the remembered URL for key, if one is still valid.
func (c *presignCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !time.Now().Before(e.until) {
		return "", false
	}
	return e.url, true
}

// remembers url for key until the given instant, pruning what has expired
// and, past the cap, whatever the map iteration reaches first
func (c *presignCache) put(key, url string, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]presignEntry{}
	}
	now := time.Now()
	for k, e := range c.entries {
		if !now.Before(e.until) || len(c.entries) >= presignMaxEntries {
			delete(c.entries, k)
		}
	}
	c.entries[key] = presignEntry{url: url, until: until}
}
