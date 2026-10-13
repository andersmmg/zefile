package checksum

import (
	"context"
	"testing"

	"github.com/krishna2206/zefile/internal/db"
	"github.com/krishna2206/zefile/internal/storage"
)

func TestValidHash(t *testing.T) {
	good := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if !ValidHash(good) {
		t.Error("the digest of the empty file was refused")
	}
	for name, bad := range map[string]string{
		"empty":       "",
		"short":       "abc",
		"one over":    good + "0",
		"uppercase":   "E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855",
		"non-hex":     "zzb0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"with a gap":  " e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85",
		"with a dash": "e3b0c44298fc1c149afbf4c8996fb924-7ae41e4649b934ca495991b7852b855",
	} {
		if ValidHash(bad) {
			t.Errorf("%s digest was accepted", name)
		}
	}
}

func TestStoreClaimRefusesMalformedDigests(t *testing.T) {
	database, err := db.Open(context.Background(), db.Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	s := New(database, nil)
	p, err := storage.ParsePath("/f.bin")
	if err != nil {
		t.Fatalf("parse path: %v", err)
	}
	if _, err := s.StoreClaim(context.Background(), p, "not-a-digest"); err == nil {
		t.Error("StoreClaim accepted a malformed claim")
	}
}
