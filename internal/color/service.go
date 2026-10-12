// Package color stores the swatch a user sets on an entry.
//
// they are per user: labels carry no access, so nothing here consults the ACL.
package color

import (
	"context"
	"slices"

	"github.com/krishna2206/zefile/internal/db"
	"github.com/krishna2206/zefile/internal/db/sqlcgen"
)

// Presets is the palette, in the order the menu offers it. Ids are stable:
// they are what gets stored.
var Presets = []string{"red", "orange", "yellow", "green", "teal", "cyan", "blue", "purple", "pink"}

// Valid reports whether id names a palette colour.
func Valid(id string) bool {
	return slices.Contains(Presets, id)
}

// Service reads and writes the entry_colors table.
type Service struct {
	reads  *sqlcgen.Queries
	writes *sqlcgen.Queries
}

// New returns a Service backed by database.
func New(database *db.DB) *Service {
	return &Service{
		reads:  sqlcgen.New(database.Read),
		writes: sqlcgen.New(database.Write),
	}
}

// Set labels path for one user, replacing any colour already there
func (s *Service) Set(ctx context.Context, userID int64, path, parent, id string) error {
	return s.writes.SetEntryColor(ctx, sqlcgen.SetEntryColorParams{
		Path:   path,
		Parent: parent,
		UserID: userID,
		Color:  id,
	})
}

// Clear removes one user's label
func (s *Service) Clear(ctx context.Context, userID int64, path string) error {
	_, err := s.writes.DeleteEntryColor(ctx, sqlcgen.DeleteEntryColorParams{
		UserID: userID,
		Path:   path,
	})
	return err
}

// ForDirectory returns one user's colours for the children of dir, by path.
func (s *Service) ForDirectory(ctx context.Context, userID int64, dir string) (map[string]string, error) {
	rows, err := s.reads.ListEntryColors(ctx, sqlcgen.ListEntryColorsParams{
		UserID: userID,
		Parent: dir,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.Path] = row.Color
	}
	return out, nil
}

// ForPaths returns one user's colours for a batch of paths, for results that
// span folders.
func (s *Service) ForPaths(ctx context.Context, userID int64, paths []string) (map[string]string, error) {
	if len(paths) == 0 {
		return map[string]string{}, nil
	}
	rows, err := s.reads.GetEntryColorsForPaths(ctx, sqlcgen.GetEntryColorsForPathsParams{
		UserID: userID,
		Paths:  paths,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.Path] = row.Color
	}
	return out, nil
}

// Relocate follows a rename across a subtree, for every user.
func (s *Service) Relocate(ctx context.Context, from, to string) error {
	return s.writes.MoveEntryColorsSubtree(ctx, sqlcgen.MoveEntryColorsSubtreeParams{
		ToPath:   to,
		FromPath: from,
	})
}

// Forget drops every user's label at path and beneath it.
func (s *Service) Forget(ctx context.Context, path string) error {
	return s.writes.DeleteEntryColorsSubtree(ctx, path)
}
