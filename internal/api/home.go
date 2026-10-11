package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/krishna2206/zefile/internal/acl"
	"github.com/krishna2206/zefile/internal/auth"
	"github.com/krishna2206/zefile/internal/storage"
)

// MembersDirName is the top-level directory homes live under.
const MembersDirName = "members"

func homeFor(username string) (storage.Path, error) {
	return storage.ParsePath("/" + MembersDirName + "/" + username)
}

func isMembersTop(p storage.Path) bool {
	s := p.String()
	return s == "/"+MembersDirName || p.Parent().String() == "/"+MembersDirName
}

func guardMembersTop(w http.ResponseWriter, r *http.Request, paths ...storage.Path) bool {
	for _, p := range paths {
		if isMembersTop(p) {
			writeProblem(w, r, http.StatusForbidden, CodePermissionDenied,
				"Managed directory", "The members directory and its user folders are created automatically; manage the files inside them instead.")
			return false
		}
	}
	return true
}

// ensureHome creates the member's home directory and grant.
func (s *Server) ensureHome(ctx context.Context, user auth.User) error {
	if user.IsAdmin {
		return nil
	}
	home, err := homeFor(user.Username)
	if err != nil {
		return fmt.Errorf("home: bad username %q: %w", user.Username, err)
	}
	if _, err := s.acl.Grant(ctx, acl.Rule{
		SubjectType: acl.SubjectUser,
		SubjectID:   user.ID,
		Path:        home,
		Perms:       acl.PermAll,
		Recursive:   true,
	}); err != nil {
		return fmt.Errorf("home: grant %s: %w", home, err)
	}

	subject, err := s.acl.LoadSubject(ctx, user.ID, user.IsAdmin)
	if err != nil {
		return fmt.Errorf("home: load subject: %w", err)
	}
	uctx := acl.NewContext(ctx, subject)

	if _, err := s.fs.Stat(uctx, home); err == nil {
		return nil
	} else if !errors.Is(err, storage.ErrNotExist) {
		return fmt.Errorf("home: stat %s: %w", home, err)
	}
	if err := s.fs.MkdirAll(uctx, home); err != nil {
		if errors.Is(err, storage.ErrExist) {
			return nil
		}
		return fmt.Errorf("home: mkdir %s: %w", home, err)
	}
	if err := s.acl.SetOwner(ctx, home, user.ID); err != nil {
		return fmt.Errorf("home: own %s: %w", home, err)
	}
	return nil
}
