// Package checksum computes and caches SHA-256 digests of files.
//
// Hashing a large file is read-heavy, so it runs as a background job and the
// result is cached, keyed by path and invalidated whenever the file's size or
// modification time changes. The filesystem stays the authority: a stale cache
// entry is simply recomputed, never trusted over the disk.
package checksum

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/krishna2206/zefile/internal/db"
	"github.com/krishna2206/zefile/internal/db/sqlcgen"
	"github.com/krishna2206/zefile/internal/storage"
)

// Algorithm is the only digest offered.
const Algorithm = "sha256"

// Where a digest came from
const (
	SourceCompute = "compute" // this server hashed the bytes
	SourceStore   = "store"   // the object store holds the whole-object value
	SourceClient  = "client"  // the browser's claim; unverified here
)

// Payload is the background-job request. The caller is recorded so the worker
// reads the file with the same authority, honouring the permission model.
type Payload struct {
	Path    string `json:"path"`
	UserID  int64  `json:"user_id"`
	IsAdmin bool   `json:"is_admin"`
}

// Checksum and source
type Checksum struct {
	Path       string    `json:"path"`
	Algorithm  string    `json:"algorithm"`
	Hash       string    `json:"hash"`
	Size       int64     `json:"size"`
	ComputedAt time.Time `json:"computed_at"`
	Source     string    `json:"source"`
}

// Service computes and caches checksums.
type Service struct {
	reads  *sqlcgen.Queries
	writes *sqlcgen.Queries
	fs     storage.FS
	now    func() time.Time
}

// New builds a Service over the database and storage.
func New(database *db.DB, fs storage.FS) *Service {
	return &Service{
		reads:  sqlcgen.New(database.Read),
		writes: sqlcgen.New(database.Write),
		fs:     fs,
		now:    time.Now,
	}
}

// Cached returns the stored digest if it is still valid for the file: the cached
// size and modification time must match the file on disk, or it is stale and
// reported as absent (ok=false). Read permission on the path is required.
func (s *Service) Cached(ctx context.Context, p storage.Path) (Checksum, bool, error) {
	info, err := s.fs.Stat(ctx, p)
	if err != nil {
		return Checksum{}, false, err
	}
	if info.IsDir {
		return Checksum{}, false, storage.ErrIsDir
	}

	row, err := s.reads.GetChecksum(ctx, p.String())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Checksum{}, false, nil
		}
		return Checksum{}, false, fmt.Errorf("checksum: look up: %w", err)
	}
	if row.Size != info.Size || row.ModifiedAt != info.ModTime.Unix() {
		return Checksum{}, false, nil // the file changed since; recompute
	}
	return toChecksum(row), true, nil
}

// Compute reads the file, hashes it, and caches the result. It is called from
// the background job so a large file never blocks a request.
func (s *Service) Compute(ctx context.Context, p storage.Path) (Checksum, error) {
	info, err := s.fs.Stat(ctx, p)
	if err != nil {
		return Checksum{}, err
	}
	if info.IsDir {
		return Checksum{}, storage.ErrIsDir
	}

	f, err := s.fs.Open(ctx, p)
	if err != nil {
		return Checksum{}, err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return Checksum{}, fmt.Errorf("checksum: read %s: %w", p, err)
	}
	sum := hex.EncodeToString(h.Sum(nil))

	if err := s.writes.UpsertChecksum(ctx, sqlcgen.UpsertChecksumParams{
		Path:       p.String(),
		Algo:       Algorithm,
		Hash:       sum,
		Size:       info.Size,
		ModifiedAt: info.ModTime.Unix(),
		ComputedAt: s.now().Unix(),
		Source:     SourceCompute,
	}); err != nil {
		return Checksum{}, fmt.Errorf("checksum: store: %w", err)
	}
	return Checksum{
		Path:       p.String(),
		Algorithm:  Algorithm,
		Hash:       sum,
		Size:       info.Size,
		ComputedAt: s.now().UTC(),
		Source:     SourceCompute,
	}, nil
}

// ValidHash reports whether s is a lowercase SHA-256 hex digest.
func ValidHash(s string) bool {
	if len(s) != 2*sha256.Size {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *Service) StoreClaim(ctx context.Context, p storage.Path, hash string) (Checksum, error) {
	if !ValidHash(hash) {
		return Checksum{}, fmt.Errorf("checksum: claim is not a sha256 hex digest")
	}
	info, err := s.fs.Stat(ctx, p)
	if err != nil {
		return Checksum{}, err
	}
	if info.IsDir {
		return Checksum{}, storage.ErrIsDir
	}
	if err := s.writes.UpsertChecksum(ctx, sqlcgen.UpsertChecksumParams{
		Path:       p.String(),
		Algo:       Algorithm,
		Hash:       hash,
		Size:       info.Size,
		ModifiedAt: info.ModTime.Unix(),
		ComputedAt: s.now().Unix(),
		Source:     SourceClient,
	}); err != nil {
		return Checksum{}, fmt.Errorf("checksum: store claim: %w", err)
	}
	return Checksum{
		Path:       p.String(),
		Algorithm:  Algorithm,
		Hash:       hash,
		Size:       info.Size,
		ComputedAt: s.now().UTC(),
		Source:     SourceClient,
	}, nil
}

func (s *Service) FromStore(ctx context.Context, p storage.Path) (Checksum, bool, error) {
	store, ok := s.fs.(storage.ChecksumStore)
	if !ok {
		slog.WarnContext(ctx, "checksum: storage backend offers no stored checksums",
			"path", p.String())
		return Checksum{}, false, nil
	}
	info, err := s.fs.Stat(ctx, p)
	if err != nil {
		return Checksum{}, false, err
	}
	if info.IsDir {
		return Checksum{}, false, storage.ErrIsDir
	}

	sum, err := store.ObjectChecksum(ctx, p)
	if err != nil {
		if errors.Is(err, storage.ErrNotSupported) {
			return Checksum{}, false, nil
		}
		return Checksum{}, false, fmt.Errorf("checksum: store: %w", err)
	}
	if err := s.writes.UpsertChecksum(ctx, sqlcgen.UpsertChecksumParams{
		Path:       p.String(),
		Algo:       Algorithm,
		Hash:       sum,
		Size:       info.Size,
		ModifiedAt: info.ModTime.Unix(),
		ComputedAt: s.now().Unix(),
		Source:     SourceStore,
	}); err != nil {
		return Checksum{}, false, fmt.Errorf("checksum: store: %w", err)
	}
	return Checksum{
		Path:       p.String(),
		Algorithm:  Algorithm,
		Hash:       sum,
		Size:       info.Size,
		ComputedAt: s.now().UTC(),
		Source:     SourceStore,
	}, true, nil
}

func toChecksum(row sqlcgen.Checksum) Checksum {
	return Checksum{
		Path:       row.Path,
		Algorithm:  row.Algo,
		Hash:       row.Hash,
		Size:       row.Size,
		ComputedAt: time.Unix(row.ComputedAt, 0).UTC(),
		Source:     row.Source,
	}
}
