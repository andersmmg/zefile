package quota

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/krishna2206/zefile/internal/storage"
)

type mapOwners map[string]int64

func (m mapOwners) Owners(context.Context) (map[string]int64, error) { return m, nil }

type mapQuotas map[int64]int64

func (m mapQuotas) UserQuota(_ context.Context, id int64) (int64, error) { return m[id], nil }

// noWalk hides Walk behind the FS interface, for the unknown-usage path.
type noWalk struct{ storage.FS }

func seed(t *testing.T, root, name string, size int) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

func openLocal(t *testing.T, root string) storage.FS {
	t.Helper()
	fs, err := storage.Open(storage.Config{Root: root, Reserve: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs
}

func TestSnapshotAttributesOwners(t *testing.T) {
	root := t.TempDir()
	seed(t, root, "a.bin", 100)
	seed(t, root, "sub/b.bin", 50)
	seed(t, root, "loose.bin", 25)
	seed(t, root, ".zefile-trash/gone.bin", 1000)
	seed(t, root, ".zefile-thumbs/deadbeef.jpg", 1000)

	fs := openLocal(t, root)
	svc := New(fs, 0, mapOwners{"/a.bin": 1, "/sub/b.bin": 2, "/missing.bin": 3}, mapQuotas{})

	snap, err := svc.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.TotalBytes != 175 {
		t.Errorf("TotalBytes = %d, want 175 (reserved excluded)", snap.TotalBytes)
	}
	if snap.Files != 3 {
		t.Errorf("Files = %d, want 3", snap.Files)
	}
	if snap.ByUser[1] != 100 || snap.ByUser[2] != 50 {
		t.Errorf("ByUser = %v, want map[1:100 2:50]", snap.ByUser)
	}
	if _, ok := snap.ByUser[3]; ok {
		t.Errorf("ByUser has orphan owner 3: %v", snap.ByUser)
	}
}

func TestCheckCreate(t *testing.T) {
	root := t.TempDir()
	seed(t, root, "a.bin", 100)
	fs := openLocal(t, root)
	ctx := context.Background()

	owners := mapOwners{"/a.bin": 1}
	cases := []struct {
		name        string
		quotas      mapQuotas
		serverQuota int64
		userID      int64
		size        int64
		wantErr     bool
	}{
		{"unlimited all", mapQuotas{}, 0, 1, 1 << 40, false},
		{"user under quota", mapQuotas{1: 200}, 0, 1, 100, false},
		{"user exact fit allowed", mapQuotas{1: 200}, 0, 1, 100, false},
		{"user over quota", mapQuotas{1: 150}, 0, 1, 100, true},
		{"other user unaffected", mapQuotas{2: 10}, 0, 1, 100, false},
		{"instance exact fit allowed", mapQuotas{}, 200, 1, 100, false},
		{"instance over quota", mapQuotas{}, 150, 1, 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(fs, tc.serverQuota, owners, tc.quotas)
			err := svc.CheckCreate(ctx, tc.userID, tc.size)
			if tc.wantErr && !errors.Is(err, ErrQuotaExceeded) {
				t.Errorf("CheckCreate = %v, want ErrQuotaExceeded", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("CheckCreate = %v, want nil", err)
			}
		})
	}
}

func TestSnapshotUnknownWithoutWalk(t *testing.T) {
	root := t.TempDir()
	seed(t, root, "a.bin", 100)
	inner, err := storage.Open(storage.Config{Root: root, Reserve: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })

	svc := New(noWalk{inner}, 0, mapOwners{}, mapQuotas{})
	snap, err := svc.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.TotalBytes != 0 || snap.Files != 0 {
		t.Errorf("Snapshot = %+v, want zeros for a backend without Walk", snap)
	}
	if err := svc.CheckCreate(context.Background(), 1, 100); err != nil {
		t.Errorf("CheckCreate = %v, want nil (nothing known, nothing limited)", err)
	}
}

func TestUserUsage(t *testing.T) {
	root := t.TempDir()
	seed(t, root, "a.bin", 100)
	fs := openLocal(t, root)
	svc := New(fs, 0, mapOwners{"/a.bin": 7}, mapQuotas{})

	got, err := svc.UserUsage(context.Background(), 7)
	if err != nil || got != 100 {
		t.Errorf("UserUsage(7) = %d, %v; want 100, nil", got, err)
	}
	got, err = svc.UserUsage(context.Background(), 8)
	if err != nil || got != 0 {
		t.Errorf("UserUsage(8) = %d, %v; want 0, nil", got, err)
	}
}
