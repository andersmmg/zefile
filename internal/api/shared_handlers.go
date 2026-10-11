package api

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/krishna2206/zefile/internal/storage"
)

type sharedWithMeResponse struct {
	Entries []entryResponse `json:"entries"`
}

func (s *Server) handleSharedWithMe(w http.ResponseWriter, r *http.Request) {
	c, ok := callerFrom(r.Context())
	if !ok {
		writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Not signed in", "")
		return
	}
	out := sharedWithMeResponse{Entries: []entryResponse{}}
	if c.user.IsAdmin {
		writeJSON(w, r, http.StatusOK, out)
		return
	}

	paths, err := s.acl.SharedPaths(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	home, err := homeFor(c.user.Username)
	if err != nil {
		writeError(w, r, err)
		return
	}
	homePrefix := home.String() + "/"
	infos := make([]storage.FileInfo, 0, len(paths))
	for _, raw := range paths {
		if raw == home.String() || strings.HasPrefix(raw, homePrefix) {
			continue
		}
		p, err := storage.ParsePath(raw)
		if err != nil {
			continue
		}
		info, err := s.fs.Stat(r.Context(), p)
		if err != nil {
			if errors.Is(err, storage.ErrNotExist) || errors.Is(err, storage.ErrPermission) {
				continue
			}
			writeError(w, r, err)
			return
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].IsDir != infos[j].IsDir {
			return infos[i].IsDir
		}
		return infos[i].Name < infos[j].Name
	})
	for _, info := range infos {
		out.Entries = append(out.Entries, toEntryResponse(info))
	}
	writeJSON(w, r, http.StatusOK, out)
}
