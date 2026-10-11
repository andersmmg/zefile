package quota

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/krishna2206/zefile/internal/storage"
)

const snapshotTTL = time.Minute

// ErrQuotaExceeded is returned when an upload would exceed a quota.
var ErrQuotaExceeded = fmt.Errorf("quota: storage quota exceeded")

// Snapshot is one accounting pass over the tree.
type Snapshot struct {
	TotalBytes int64
	Files      int64
	ByUser     map[int64]int64
}

// Owners lists ownership rows by path.
type Owners interface {
	Owners(ctx context.Context) (map[string]int64, error)
}

// Quotas reads per-account limits in bytes. Zero means unlimited.
type Quotas interface {
	UserQuota(ctx context.Context, userID int64) (int64, error)
}

// Service accounts bytes and refuses uploads past configured limits.
type Service struct {
	fs          storage.FS
	serverQuota int64
	owners      Owners
	quotas      Quotas
	ttl         time.Duration

	mu       sync.Mutex
	cached   Snapshot
	cachedAt time.Time
}

// New builds a Service.
func New(fs storage.FS, serverQuota int64, owners Owners, quotas Quotas) *Service {
	return &Service{fs: fs, serverQuota: serverQuota, owners: owners, quotas: quotas, ttl: snapshotTTL}
}

// Snapshot returns the cached accounting, walking the tree when stale.
func (s *Service) Snapshot(ctx context.Context) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.cachedAt.IsZero() && time.Since(s.cachedAt) < s.ttl {
		return s.cached, nil
	}
	snap, err := s.walk(ctx)
	if err != nil {
		if !s.cachedAt.IsZero() {
			return s.cached, nil
		}
		return Snapshot{}, err
	}
	s.cached, s.cachedAt = snap, time.Now()
	return snap, nil
}

// ServerQuota returns the instance cap in bytes. Zero means unlimited.
func (s *Service) ServerQuota() int64 { return s.serverQuota }

// Invalidate drops the cached snapshot.
func (s *Service) Invalidate() {
	s.mu.Lock()
	s.cachedAt = time.Time{}
	s.mu.Unlock()
}

// UserUsage reports one account's owned bytes.
func (s *Service) UserUsage(ctx context.Context, userID int64) (int64, error) {
	snap, err := s.Snapshot(ctx)
	if err != nil {
		return 0, err
	}
	return snap.ByUser[userID], nil
}

// CheckCreate refuses a declared upload that would breach a quota.
func (s *Service) CheckCreate(ctx context.Context, userID, size int64) error {
	if size < 0 {
		size = 0
	}
	snap, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	if s.quotas != nil {
		quota, err := s.quotas.UserQuota(ctx, userID)
		if err != nil {
			return err
		}
		if quota > 0 && snap.ByUser[userID]+size > quota {
			return fmt.Errorf("%w: account holds %d bytes and the upload adds %d past its %d-byte limit",
				ErrQuotaExceeded, snap.ByUser[userID], size, quota)
		}
	}
	if s.serverQuota > 0 && snap.TotalBytes+size > s.serverQuota {
		return fmt.Errorf("%w: the instance holds %d bytes and the upload adds %d past its %d-byte limit",
			ErrQuotaExceeded, snap.TotalBytes, size, s.serverQuota)
	}
	return nil
}

func (s *Service) walk(ctx context.Context) (Snapshot, error) {
	walker, ok := s.fs.(storage.Walker)
	if !ok {
		return Snapshot{}, nil
	}
	var owners map[string]int64
	if s.owners != nil {
		var err error
		owners, err = s.owners.Owners(ctx)
		if err != nil {
			return Snapshot{}, fmt.Errorf("quota: list owners: %w", err)
		}
	}
	snap := Snapshot{ByUser: map[int64]int64{}}
	err := walker.Walk(ctx, func(path string, size int64) error {
		if size < 0 {
			size = 0
		}
		snap.TotalBytes += size
		snap.Files++
		if id, ok := owners[path]; ok {
			snap.ByUser[id] += size
		}
		return nil
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("quota: walk: %w", err)
	}
	return snap, nil
}
