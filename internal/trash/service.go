// Package trash implements reversible deletion.
package trash

import (
	"context"
	"database/sql"
	"errors"
	"path"
	"time"

	"github.com/krishna2206/zefile/internal/db"
	"github.com/krishna2206/zefile/internal/db/sqlcgen"
	"github.com/krishna2206/zefile/internal/storage"
)

// ErrNotFound means the trashed item is unknown or already gone.
var ErrNotFound = errors.New("trash: no such item")

// Item is a trashed entry as presented to a caller.
type Item struct {
	ID           int64
	Name         string
	OriginalPath string
	IsDir        bool
	Size         int64
	DeletedAt    time.Time
	DeletedBy    int64
}

// Service manages the trash.
type Service struct {
	fs     storage.Trasher
	reads  *sqlcgen.Queries
	writes *sqlcgen.Queries
	now    func() time.Time
}

// New builds a Service over the given database and storage.
func New(database *db.DB, fs storage.Trasher) *Service {
	return &Service{
		fs:     fs,
		reads:  sqlcgen.New(database.Read),
		writes: sqlcgen.New(database.Write),
		now:    time.Now,
	}
}

// Trash moves an entry into the trash.
func (s *Service) Trash(ctx context.Context, userID int64, p storage.Path) error {
	id, err := s.fs.Trash(ctx, p)
	if err != nil {
		return err
	}

	isDir := false
	if info, err := s.fs.TrashStat(ctx, id); err == nil {
		isDir = info.IsDir()
	}

	_, err = s.writes.CreateTrash(ctx, sqlcgen.CreateTrashParams{
		TrashName:    string(id),
		OriginalPath: p.String(),
		DeletedBy:    sql.NullInt64{Int64: userID, Valid: userID != 0},
		DeletedAt:    s.now().Unix(),
		IsDir:        boolToInt(isDir),
	})
	if err != nil {
		_ = s.fs.Restore(ctx, id, p)
		return err
	}
	return nil
}

// List returns trashed items, newest first.
func (s *Service) List(ctx context.Context) ([]Item, error) {
	rows, err := s.reads.ListTrash(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(rows))
	for _, r := range rows {
		var size int64
		if info, err := s.fs.TrashStat(ctx, storage.TrashID(r.TrashName)); err == nil {
			size = info.Size()
		}
		items = append(items, Item{
			ID:           r.ID,
			Name:         path.Base(r.OriginalPath),
			OriginalPath: r.OriginalPath,
			IsDir:        r.IsDir != 0,
			Size:         size,
			DeletedAt:    time.Unix(r.DeletedAt, 0),
			DeletedBy:    r.DeletedBy.Int64,
		})
	}
	return items, nil
}

// ListFor returns trashed items the caller may see.
func (s *Service) ListFor(ctx context.Context, userID int64, isAdmin bool) ([]Item, error) {
	items, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	if isAdmin {
		return items, nil
	}
	mine := items[:0]
	for _, it := range items {
		if it.DeletedBy != 0 && it.DeletedBy == userID {
			mine = append(mine, it)
		}
	}
	return mine, nil
}

func ownerOK(deletedBy, userID int64, isAdmin bool) bool {
	return isAdmin || (userID != 0 && deletedBy != 0 && deletedBy == userID)
}

// OriginalPathFor reports where a trashed item would restore to.
func (s *Service) OriginalPathFor(ctx context.Context, id, userID int64, isAdmin bool) (storage.Path, error) {
	row, err := s.reads.GetTrash(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storage.Path{}, ErrNotFound
		}
		return storage.Path{}, err
	}
	if !ownerOK(row.DeletedBy.Int64, userID, isAdmin) {
		return storage.Path{}, ErrNotFound
	}
	dest, err := storage.ParsePath(row.OriginalPath)
	if err != nil {
		return storage.Path{}, err
	}
	return dest, nil
}

// Restore returns a trashed item to its original location.
func (s *Service) Restore(ctx context.Context, id int64) (storage.Path, error) {
	return s.RestoreFor(ctx, id, 0, true)
}

// RestoreFor restores what the caller may see.
func (s *Service) RestoreFor(ctx context.Context, id, userID int64, isAdmin bool) (storage.Path, error) {
	row, err := s.reads.GetTrash(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storage.Path{}, ErrNotFound
		}
		return storage.Path{}, err
	}
	if !ownerOK(row.DeletedBy.Int64, userID, isAdmin) {
		return storage.Path{}, ErrNotFound
	}
	dest, err := storage.ParsePath(row.OriginalPath)
	if err != nil {
		return storage.Path{}, err
	}
	if err := s.fs.Restore(ctx, storage.TrashID(row.TrashName), dest); err != nil {
		return storage.Path{}, err
	}
	if err := s.writes.DeleteTrash(ctx, id); err != nil {
		return dest, err
	}
	return dest, nil
}

// Purge removes one trashed item for good.
func (s *Service) Purge(ctx context.Context, id int64) error {
	return s.PurgeFor(ctx, id, 0, true)
}

// PurgeFor purges what the caller may see.
func (s *Service) PurgeFor(ctx context.Context, id, userID int64, isAdmin bool) error {
	row, err := s.reads.GetTrash(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !ownerOK(row.DeletedBy.Int64, userID, isAdmin) {
		return ErrNotFound
	}
	if err := s.fs.PurgeTrash(ctx, storage.TrashID(row.TrashName)); err != nil {
		return err
	}
	return s.writes.DeleteTrash(ctx, id)
}

// PurgeExpired purges items deleted before the cutoff.
func (s *Service) PurgeExpired(ctx context.Context, before time.Time) (int, error) {
	items, err := s.List(ctx)
	if err != nil {
		return 0, err
	}
	purged := 0
	for _, it := range items {
		if it.DeletedAt.Before(before) {
			if err := s.Purge(ctx, it.ID); err != nil {
				return purged, err
			}
			purged++
		}
	}
	return purged, nil
}

// Empty purges every trashed item.
func (s *Service) Empty(ctx context.Context) error {
	return s.EmptyFor(ctx, 0, true)
}

// EmptyFor empties what the caller may see.
func (s *Service) EmptyFor(ctx context.Context, userID int64, isAdmin bool) error {
	rows, err := s.reads.ListTrash(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if !ownerOK(r.DeletedBy.Int64, userID, isAdmin) {
			continue
		}
		if err := s.fs.PurgeTrash(ctx, storage.TrashID(r.TrashName)); err != nil {
			return err
		}
		if err := s.writes.DeleteTrash(ctx, r.ID); err != nil {
			return err
		}
	}
	return nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
