package storage

import (
	"context"
	"errors"
	"fmt"
)

// Shared authorisation steps for every backend: reserved-name checks, Guard
// queries and batch filtering.

// authorize runs the reserved-name check and the Guard, in that order, and
// fails closed on any Guard error other than a plain refusal.
func authorize(ctx context.Context, g Guard, op Op, p Path) error {
	if isReserved(p) {
		return ErrReserved
	}
	if err := g.Authorize(ctx, op, p); err != nil {
		if errors.Is(err, ErrPermission) {
			return ErrPermission
		}
		return fmt.Errorf("storage: authorisation failed: %w", err)
	}
	return nil
}

// authorizeList runs the reserved-name check and the list-specific form of
// authorisation for one directory, reporting whether verdicts resolve
// through a [ListGuard] (traverse) or plain read filtering.
func authorizeList(ctx context.Context, g Guard, p Path) (traverse bool, err error) {
	if isReserved(p) {
		return false, ErrReserved
	}

	if lister, ok := g.(ListGuard); ok {
		can, err := lister.CanList(ctx, p)
		if err != nil {
			return false, fmt.Errorf("storage: authorisation failed: %w", err)
		}
		if !can {
			return false, ErrPermission
		}
		return true, nil
	}
	if err := g.Authorize(ctx, OpRead, p); err != nil {
		if errors.Is(err, ErrPermission) {
			return false, ErrPermission
		}
		return false, fmt.Errorf("storage: authorisation failed: %w", err)
	}
	return false, nil
}

// filterVisible keeps the entries a ListGuard says the caller may see.
func filterVisible(ctx context.Context, lister ListGuard, entries []FileInfo) ([]FileInfo, error) {
	if len(entries) == 0 {
		return entries, nil
	}
	paths := make([]Path, len(entries))
	for i, entry := range entries {
		paths[i] = entry.Path
	}

	verdicts, err := lister.VisibleChildren(ctx, paths)
	if err != nil {
		return nil, fmt.Errorf("storage: authorisation failed: %w", err)
	}
	if len(verdicts) != len(entries) {
		return nil, fmt.Errorf("storage: guard returned %d verdicts for %d paths", len(verdicts), len(entries))
	}

	visible := entries[:0]
	for i, ok := range verdicts {
		if ok {
			visible = append(visible, entries[i])
		}
	}
	return visible, nil
}

// filterReadable drops entries the caller may not see, fetching verdicts
// in one batch.
func filterReadable(ctx context.Context, g Guard, entries []FileInfo) ([]FileInfo, error) {
	if len(entries) == 0 {
		return entries, nil
	}

	paths := make([]Path, len(entries))
	for i, entry := range entries {
		paths[i] = entry.Path
	}

	verdicts, err := g.Permitted(ctx, OpRead, paths)
	if err != nil {
		return nil, fmt.Errorf("storage: authorisation failed: %w", err)
	}
	if len(verdicts) != len(entries) {
		return nil, fmt.Errorf("storage: guard returned %d verdicts for %d paths", len(verdicts), len(entries))
	}

	visible := entries[:0]
	for i, ok := range verdicts {
		if ok {
			visible = append(visible, entries[i])
		}
	}
	return visible, nil
}
