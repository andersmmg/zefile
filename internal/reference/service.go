// Package reference stores the target of an entry that is a reference, similar
// to a symlink. Per-user.
package reference

import (
	"context"

	"github.com/krishna2206/zefile/internal/db"
	"github.com/krishna2206/zefile/internal/db/sqlcgen"
)

type Service struct {
	reads  *sqlcgen.Queries
	writes *sqlcgen.Queries
}

func New(database *db.DB) *Service {
	return &Service{
		reads:  sqlcgen.New(database.Read),
		writes: sqlcgen.New(database.Write),
	}
}

func (s *Service) Set(ctx context.Context, userID int64, path, parent, target string) error {
	return s.writes.SetReference(ctx, sqlcgen.SetReferenceParams{
		Path:   path,
		Parent: parent,
		UserID: userID,
		Target: target,
	})
}

func (s *Service) Clear(ctx context.Context, userID int64, path string) error {
	_, err := s.writes.DeleteReference(ctx, sqlcgen.DeleteReferenceParams{
		UserID: userID,
		Path:   path,
	})
	return err
}

func (s *Service) Get(ctx context.Context, userID int64, path string) (string, bool, error) {
	rows, err := s.reads.GetReferencesForPaths(ctx, sqlcgen.GetReferencesForPathsParams{
		UserID: userID,
		Paths:  []string{path},
	})
	if err != nil {
		return "", false, err
	}
	for _, row := range rows {
		if row.Path == path {
			return row.Target, true, nil
		}
	}
	return "", false, nil
}

func (s *Service) ForDirectory(ctx context.Context, userID int64, dir string) (map[string]string, error) {
	rows, err := s.reads.ListReferences(ctx, sqlcgen.ListReferencesParams{
		UserID: userID,
		Parent: dir,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.Path] = row.Target
	}
	return out, nil
}

func (s *Service) ForPaths(ctx context.Context, userID int64, paths []string) (map[string]string, error) {
	if len(paths) == 0 {
		return map[string]string{}, nil
	}
	rows, err := s.reads.GetReferencesForPaths(ctx, sqlcgen.GetReferencesForPathsParams{
		UserID: userID,
		Paths:  paths,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.Path] = row.Target
	}
	return out, nil
}

func (s *Service) ClearIfPresent(ctx context.Context, userID int64, path string) (bool, error) {
	_, ok, err := s.Get(ctx, userID, path)
	if err != nil || !ok {
		return ok, err
	}
	if err := s.Clear(ctx, userID, path); err != nil {
		return true, err
	}
	return true, nil
}

func (s *Service) Move(ctx context.Context, userID int64, from, to, parent string) (bool, error) {
	target, ok, err := s.Get(ctx, userID, from)
	if err != nil || !ok {
		return ok, err
	}
	if err := s.Set(ctx, userID, to, parent, target); err != nil {
		return true, err
	}
	_, err = s.writes.DeleteReference(ctx, sqlcgen.DeleteReferenceParams{
		UserID: userID,
		Path:   from,
	})
	return true, err
}

func (s *Service) Relocate(ctx context.Context, from, to string) error {
	if err := s.writes.MoveReferencesSubtree(ctx, sqlcgen.MoveReferencesSubtreeParams{
		ToPath:   to,
		FromPath: from,
	}); err != nil {
		return err
	}
	return s.writes.UpdateReferencesTargetSubtree(ctx, sqlcgen.UpdateReferencesTargetSubtreeParams{
		ToPath:   to,
		FromPath: from,
	})
}

func (s *Service) Forget(ctx context.Context, path string) error {
	return s.writes.DeleteReferencesSubtree(ctx, path)
}
