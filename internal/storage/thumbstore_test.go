package storage

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

const testThumbKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func thumbStores(t *testing.T) map[string]ThumbStore {
	t.Helper()
	return map[string]ThumbStore{
		"local": mustOpen(t, Config{Root: t.TempDir(), Reserve: 1}),
		"s3":    newTestS3(t, newStubS3(), nil),
	}
}

func TestThumbStoreRoundTrip(t *testing.T) {
	for name, store := range thumbStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			jpeg := []byte{0xff, 0xd8, 0xff, 0xd9}

			if _, err := store.ReadThumb(ctx, testThumbKey); !errors.Is(err, ErrNotExist) {
				t.Fatalf("missing = %v, want ErrNotExist", err)
			}
			if err := store.WriteThumb(ctx, testThumbKey, jpeg); err != nil {
				t.Fatalf("Write: %v", err)
			}
			got, err := store.ReadThumb(ctx, testThumbKey)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if !bytes.Equal(got, jpeg) {
				t.Fatalf("read %v, want %v", got, jpeg)
			}
			if _, err := store.ReadThumb(ctx, "../escape"); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("bad key = %v, want ErrInvalidPath", err)
			}
			if err := store.WriteThumb(ctx, testThumbKey, make([]byte, MaxThumbBytes+1)); !errors.Is(err, ErrTooLarge) {
				t.Errorf("oversize = %v, want ErrTooLarge", err)
			}
		})
	}
}

func TestThumbsStayHidden(t *testing.T) {
	ctx := context.Background()
	local := mustOpen(t, Config{Root: t.TempDir(), Reserve: 1})
	if err := local.WriteThumb(ctx, testThumbKey, []byte{1}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	entries, err := local.List(ctx, MustParsePath("/"))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name, "thumbs") {
			t.Fatalf("listing shows %q", e.Name)
		}
	}
	if _, err := local.Stat(ctx, MustParsePath("/.zefile-thumbs")); !errors.Is(err, ErrReserved) {
		t.Fatalf("Stat reserved = %v, want ErrReserved", err)
	}
}
