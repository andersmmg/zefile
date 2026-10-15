package api

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/krishna2206/zefile/internal/acl"
	"github.com/krishna2206/zefile/internal/audit"
	"github.com/krishna2206/zefile/internal/color"
	"github.com/krishna2206/zefile/internal/content"
	"github.com/krishna2206/zefile/internal/job"
	"github.com/krishna2206/zefile/internal/storage"
)

type entryResponse struct {
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	IsDir   bool      `json:"is_dir"`
	Symlink bool      `json:"symlink,omitempty"`
	Color   string    `json:"color,omitempty"`
	Ref     string    `json:"ref,omitempty"`
}

func (s *Server) attachMeta(r *http.Request, entries []entryResponse, dir string) {
	if len(entries) == 0 {
		return
	}
	c, ok := callerFrom(r.Context())
	if !ok {
		return
	}

	s.attachColors(r, c.user.ID, entries, dir)
}

func (s *Server) attachColors(r *http.Request, userID int64, entries []entryResponse, dir string) {
	if s.colors == nil {
		return
	}

	var (
		found map[string]string
		err   error
	)
	if dir != "" {
		found, err = s.colors.ForDirectory(r.Context(), userID, dir)
	} else {
		paths := make([]string, 0, len(entries))
		for _, entry := range entries {
			paths = append(paths, entry.Path)
		}
		found, err = s.colors.ForPaths(r.Context(), userID, paths)
	}
	if err != nil {
		return
	}
	for i := range entries {
		if id := found[entries[i].Path]; id != "" {
			entries[i].Color = id
		}
	}
}

func (s *Server) injectReferences(r *http.Request, out []entryResponse, dir storage.Path) []entryResponse {
	if s.references == nil {
		return out
	}
	c, ok := callerFrom(r.Context())
	if !ok {
		return out
	}
	refs, err := s.references.ForDirectory(r.Context(), c.user.ID, dir.String())
	if err != nil {
		return out
	}
	for path, target := range refs {
		tp, err := storage.ParsePath(target)
		if err != nil {
			continue
		}
		ref := entryResponse{
			Path:    path,
			Name:    storage.MustParsePath(path).Name(),
			ModTime: time.Now().UTC(),
		}
		if info, err := s.fs.Stat(r.Context(), tp); err == nil {
			ref = toEntryResponse(info)
			ref.Path = path
			ref.Name = storage.MustParsePath(path).Name()
			ref.Ref = target
		}
		out = append(out, ref)
	}
	return out
}

// maps a reference path to the path it points at for the caller
func (s *Server) resolveReference(r *http.Request, p storage.Path) storage.Path {
	if s.references == nil {
		return p
	}
	c, ok := callerFrom(r.Context())
	if !ok {
		return p
	}
	target, found, err := s.references.Get(r.Context(), c.user.ID, p.String())
	if err != nil || !found {
		return p
	}
	tp, err := storage.ParsePath(target)
	if err != nil {
		return p
	}
	return tp
}

type listResponse struct {
	Path    string          `json:"path"`
	Entries []entryResponse `json:"entries"`
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	p, ok := pathParam(w, r)
	if !ok {
		return
	}
	p = s.resolveReference(r, p)

	entries, err := s.fs.List(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}

	if p.IsRoot() {
		if c, ok := callerFrom(r.Context()); ok && !c.user.IsAdmin {
			writeProblem(w, r, http.StatusForbidden, CodePermissionDenied,
				"No root", "Browse your home instead — the tree root is not visible to members.")
			return
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})

	out := make([]entryResponse, 0, len(entries))
	for _, entry := range entries {
		out = append(out, toEntryResponse(entry))
	}
	out = s.injectReferences(r, out, p)
	s.attachMeta(r, out, p.String())
	writeJSON(w, r, http.StatusOK, listResponse{Path: p.String(), Entries: out})
}

type countResponse struct {
	Files int64 `json:"files"`
	Dirs  int64 `json:"dirs"`
}

func (s *Server) handleCount(w http.ResponseWriter, r *http.Request) {
	p, ok := pathParam(w, r)
	if !ok {
		return
	}
	p = s.resolveReference(r, p)

	entries, err := s.fs.List(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	out := countResponse{}
	for _, entry := range entries {
		if entry.IsDir {
			out.Dirs++
		} else {
			out.Files++
		}
	}
	s.countReferences(r, p, &out)
	writeJSON(w, r, http.StatusOK, out)
}

// adds the caller's references in dir to a folder's total
func (s *Server) countReferences(r *http.Request, dir storage.Path, out *countResponse) {
	if s.references == nil {
		return
	}
	c, ok := callerFrom(r.Context())
	if !ok {
		return
	}
	refs, err := s.references.ForDirectory(r.Context(), c.user.ID, dir.String())
	if err != nil {
		return
	}
	for _, target := range refs {
		if tp, err := storage.ParsePath(target); err == nil {
			if info, err := s.fs.Stat(r.Context(), tp); err == nil && info.IsDir {
				out.Dirs++
				continue
			}
		}
		out.Files++
	}
}

func (s *Server) handleStat(w http.ResponseWriter, r *http.Request) {
	p, ok := pathParam(w, r)
	if !ok {
		return
	}

	info, err := s.fs.Stat(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	out := toEntryResponse(info)
	s.attachMeta(r, []entryResponse{out}, p.Parent().String())
	writeJSON(w, r, http.StatusOK, out)
}

type fileInfoResponse struct {
	Path    string     `json:"path"`
	Name    string     `json:"name"`
	IsDir   bool       `json:"is_dir"`
	Symlink bool       `json:"symlink,omitempty"`
	Size    int64      `json:"size"`
	ModTime time.Time  `json:"mod_time"`
	Owner   string     `json:"owner,omitempty"`
	AddedAt *time.Time `json:"added_at,omitempty"`
}

// describes one entry for the details panel
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	p, ok := pathParam(w, r)
	if !ok {
		return
	}

	target := s.resolveReference(r, p)
	info, err := s.fs.Stat(r.Context(), target)
	if err != nil {
		writeError(w, r, err)
		return
	}
	out := fileInfoResponse{
		Path:    p.String(),
		Name:    p.Name(),
		IsDir:   info.IsDir,
		Symlink: info.Symlink,
		Size:    info.Size,
		ModTime: info.ModTime.UTC(),
	}

	own, found, err := s.acl.OwnershipOf(r.Context(), target)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if found {
		at := time.Unix(own.AddedAt, 0).UTC()
		out.AddedAt = &at
		if u, err := s.auth.GetUser(r.Context(), own.OwnerID); err == nil {
			out.Owner = u.Username
		}
	}
	writeJSON(w, r, http.StatusOK, out)
}

type colorRequest struct {
	Path  string `json:"path"`
	Color string `json:"color"`
}

func (s *Server) handleSetColor(w http.ResponseWriter, r *http.Request) {
	var body colorRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	p, ok := parsePath(w, r, body.Path)
	if !ok {
		return
	}
	c, ok := callerFrom(r.Context())
	if !ok {
		writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Not signed in", "")
		return
	}
	if s.colors == nil {
		writeProblem(w, r, http.StatusNotImplemented, CodeBadRequest,
			"Colours unavailable", "This server does not store entry colours.")
		return
	}
	if body.Color != "" && !color.Valid(body.Color) {
		writeProblem(w, r, http.StatusBadRequest, CodeBadRequest,
			"Unknown colour", "Choose one of the colours the interface offers.")
		return
	}

	var err error
	if body.Color == "" {
		err = s.colors.Clear(r.Context(), c.user.ID, p.String())
	} else {
		err = s.colors.Set(r.Context(), c.user.ID, p.String(), p.Parent().String(), body.Color)
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, struct {
		Color string `json:"color"`
	}{Color: body.Color})
}

type referenceRequest struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

func (s *Server) handleSetReference(w http.ResponseWriter, r *http.Request) {
	var body referenceRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	p, ok := parsePath(w, r, body.Path)
	if !ok {
		return
	}
	target, ok := parsePath(w, r, body.Target)
	if !ok {
		return
	}
	c, ok := callerFrom(r.Context())
	if !ok {
		writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Not signed in", "")
		return
	}
	if s.references == nil {
		writeProblem(w, r, http.StatusNotImplemented, CodeBadRequest,
			"References unavailable", "This server does not store references.")
		return
	}

	// reference must be to a readable file
	if _, err := s.fs.Stat(r.Context(), target); err != nil {
		writeError(w, r, err)
		return
	}
	if allowed, err := s.acl.Allows(r.Context(), acl.PermRead, target); err != nil || !allowed {
		writeProblem(w, r, http.StatusForbidden, CodePermissionDenied,
			"Cannot reference that", "You cannot read the target, so you cannot link to it.")
		return
	}
	if !guardMembersTop(w, r, p) {
		return
	}

	// Check if the reference already exists
	_, exists, err := s.references.Get(r.Context(), c.user.ID, p.String())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if exists {
		writeProblem(w, r, http.StatusConflict, CodeConflict,
			"Already exists", "A reference is already there.")
		return
	}
	if err := s.references.Set(r.Context(), c.user.ID, p.String(), p.Parent().String(), target.String()); err != nil {
		writeError(w, r, err)
		return
	}
	// Get the target's info to return it
	info, err := s.fs.Stat(r.Context(), target)
	if err != nil {
		_ = s.references.Clear(r.Context(), c.user.ID, p.String())
		writeError(w, r, err)
		return
	}
	out := toEntryResponse(info)
	out.Path = p.String()
	out.Name = p.Name()
	out.Ref = target.String()
	writeJSON(w, r, http.StatusCreated, out)
}

// removes a reference
func (s *Server) handleClearReference(w http.ResponseWriter, r *http.Request) {
	p, ok := pathParam(w, r)
	if !ok {
		return
	}
	c, ok := callerFrom(r.Context())
	if !ok {
		writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Not signed in", "")
		return
	}
	if s.references == nil {
		writeProblem(w, r, http.StatusNotImplemented, CodeBadRequest,
			"References unavailable", "This server does not store references.")
		return
	}
	if err := s.references.Clear(r.Context(), c.user.ID, p.String()); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusNoContent, nil)
}

type pathRequest struct {
	Path string `json:"path"`
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var body pathRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	p, ok := parsePath(w, r, body.Path)
	if !ok {
		return
	}
	if !guardMembersTop(w, r, p) {
		return
	}

	if err := s.fs.MkdirAll(r.Context(), p); err != nil {
		writeError(w, r, err)
		return
	}

	info, err := s.fs.Stat(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, toEntryResponse(info))
}

type moveRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	var body moveRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	from, ok := parsePath(w, r, body.From)
	if !ok {
		return
	}
	to, ok := parsePath(w, r, body.To)
	if !ok {
		return
	}
	if !guardMembersTop(w, r, from, to) {
		return
	}

	// only move the reference
	if s.references != nil {
		c, ok := callerFrom(r.Context())
		if !ok {
			writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Not signed in", "")
			return
		}
		moved, err := s.references.Move(r.Context(), c.user.ID, from.String(), to.String(), to.Parent().String())
		if err != nil {
			writeError(w, r, err)
			return
		}
		if moved {
			refTarget, _, err := s.references.Get(r.Context(), c.user.ID, to.String())
			if err != nil {
				writeError(w, r, err)
				return
			}
			tp, err := storage.ParsePath(refTarget)
			if err != nil {
				writeError(w, r, err)
				return
			}
			info, err := s.fs.Stat(r.Context(), tp)
			if err != nil {
				writeError(w, r, err)
				return
			}
			out := toEntryResponse(info)
			out.Path = to.String()
			out.Name = to.Name()
			out.Ref = refTarget
			writeJSON(w, r, http.StatusOK, out)
			return
		}
	}

	if err := s.fs.Move(r.Context(), from, to); err != nil {
		writeError(w, r, err)
		return
	}

	if err := s.acl.MoveOwner(r.Context(), from, to); err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.acl.MoveRules(r.Context(), from, to); err != nil {
		writeError(w, r, err)
		return
	}
	if s.colors != nil {
		if err := s.colors.Relocate(r.Context(), from.String(), to.String()); err != nil {
			writeError(w, r, err)
			return
		}
	}
	// References follow the rename
	if s.references != nil {
		if err := s.references.Relocate(r.Context(), from.String(), to.String()); err != nil {
			writeError(w, r, err)
			return
		}
	}

	info, err := s.fs.Stat(r.Context(), to)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toEntryResponse(info))
}

func (s *Server) handleCopy(w http.ResponseWriter, r *http.Request) {
	var body moveRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	from, ok := parsePath(w, r, body.From)
	if !ok {
		return
	}
	to, ok := parsePath(w, r, body.To)
	if !ok {
		return
	}
	if !guardMembersTop(w, r, to) {
		return
	}

	err := s.fs.Copy(r.Context(), from, to)

	if errors.Is(err, storage.ErrIsDir) || errors.Is(err, storage.ErrTooLarge) {
		s.enqueueCopy(w, r, from, to)
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}

	if c, found := callerFrom(r.Context()); found {
		if err := s.acl.SetOwner(r.Context(), to, c.user.ID); err != nil {
			writeError(w, r, err)
			return
		}
	}

	info, err := s.fs.Stat(r.Context(), to)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, toEntryResponse(info))
}

type copyJobResponse struct {
	Job jobResponse `json:"job"`
}

func (s *Server) enqueueCopy(w http.ResponseWriter, r *http.Request, from, to storage.Path) {
	c, found := callerFrom(r.Context())
	if !found {
		writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Not signed in", "")
		return
	}
	if s.jobs == nil {
		writeError(w, r, storage.ErrIsDir) // no queue wired: report the original refusal
		return
	}

	j, err := s.jobs.Enqueue(r.Context(), job.TypeCopy, job.CopyPayload{
		From:    from.String(),
		To:      to.String(),
		UserID:  c.user.ID,
		IsAdmin: c.user.IsAdmin,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusAccepted, copyJobResponse{Job: toJobResponse(j)})
}

type extractRequest struct {
	Archive string `json:"archive"`
	Dest    string `json:"dest"`
}

// handleExtract unpacks a ZIP archive as a background job.
func (s *Server) handleExtract(w http.ResponseWriter, r *http.Request) {
	c, ok := callerFrom(r.Context())
	if !ok {
		writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Not signed in", "")
		return
	}
	var body extractRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	archive, ok := parsePath(w, r, body.Archive)
	if !ok {
		return
	}

	if _, ok := s.fs.(storage.Extractor); !ok {
		writeProblem(w, r, http.StatusNotImplemented, CodeNotSupported,
			"Not supported", "Extraction is not supported for object storage.")
		return
	}

	dest := archive.Parent()
	if body.Dest != "" {
		dest, ok = parsePath(w, r, body.Dest)
		if !ok {
			return
		}
	}
	if !guardMembersTop(w, r, dest) {
		return
	}

	info, err := s.fs.Stat(r.Context(), archive)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if info.IsDir {
		writeProblem(w, r, http.StatusBadRequest, CodeIsDirectory,
			"Not an archive", "A directory cannot be extracted.")
		return
	}

	if s.jobs == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, CodeInternal,
			"No worker", "Extraction needs the background worker.")
		return
	}
	j, err := s.jobs.Enqueue(r.Context(), job.TypeExtract, job.ExtractPayload{
		Archive: archive.String(),
		Dest:    dest.String(),
		UserID:  c.user.ID,
		IsAdmin: c.user.IsAdmin,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusAccepted, copyJobResponse{Job: toJobResponse(j)})
}

type fetchRequest struct {
	URL  string `json:"url"`
	Dir  string `json:"dir"`
	Name string `json:"name"`
}

// handleFetch downloads a URL as a background job.
func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request) {
	c, ok := callerFrom(r.Context())
	if !ok {
		writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Not signed in", "")
		return
	}
	if !s.urlFetch {
		writeProblem(w, r, http.StatusNotImplemented, CodeNotSupported,
			"Turned off", "The operator has not enabled downloading files from URLs on this server.")
		return
	}
	var body fetchRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	u, err := url.Parse(strings.TrimSpace(body.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeProblem(w, r, http.StatusBadRequest, CodeBadRequest,
			"Invalid URL", "Provide an http or https URL.")
		return
	}

	if _, ok := s.fs.(storage.Stager); !ok {
		writeProblem(w, r, http.StatusNotImplemented, CodeNotSupported,
			"Not supported", "This storage backend cannot stage a download.")
		return
	}

	dir := storage.Root
	if body.Dir != "" {
		dir, ok = parsePath(w, r, body.Dir)
		if !ok {
			return
		}
	}
	if !guardMembersTop(w, r, dir) {
		return
	}

	if s.jobs == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, CodeInternal,
			"No worker", "Downloads need the background worker.")
		return
	}
	j, err := s.jobs.Enqueue(r.Context(), job.TypeFetch, job.FetchPayload{
		URL:     u.String(),
		Dir:     dir.String(),
		Name:    strings.TrimSpace(body.Name),
		UserID:  c.user.ID,
		IsAdmin: c.user.IsAdmin,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusAccepted, copyJobResponse{Job: toJobResponse(j)})
}

// handleDelete moves an entry to the trash.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := pathParam(w, r)
	if !ok {
		return
	}
	c, ok := callerFrom(r.Context())
	if !ok {
		writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Not signed in", "")
		return
	}

	if !guardMembersTop(w, r, p) {
		return
	}
	// Deleting a reference removes only the pointer
	if s.references != nil {
		removed, err := s.references.ClearIfPresent(r.Context(), c.user.ID, p.String())
		if err != nil {
			writeError(w, r, err)
			return
		}
		if removed {
			s.audit(r, audit.ActionFileTrashed, p.String(), nil)
			writeJSON(w, r, http.StatusNoContent, nil)
			return
		}
	}
	if err := s.trash.Trash(r.Context(), c.user.ID, p); err != nil {
		writeError(w, r, err)
		return
	}

	if err := s.acl.ClearOwner(r.Context(), p); err != nil {
		writeError(w, r, err)
		return
	}
	// clear non existing file colors
	if s.colors != nil {
		if err := s.colors.Forget(r.Context(), p.String()); err != nil {
			writeError(w, r, err)
			return
		}
	}
	// references are per user and keyed by path, so a trashed subtree drops
	// its links rather than leaving rows pointing into nothing
	if s.references != nil {
		if err := s.references.Forget(r.Context(), p.String()); err != nil {
			writeError(w, r, err)
			return
		}
	}
	s.audit(r, audit.ActionFileTrashed, p.String(), nil)
	writeJSON(w, r, http.StatusNoContent, nil)
}

type linkResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// handleLink mints a short-lived download URL.
func (s *Server) handleLink(w http.ResponseWriter, r *http.Request) {
	p, ok := pathParam(w, r)
	if !ok {
		return
	}
	p = s.resolveReference(r, p)

	info, err := s.fs.Stat(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if info.IsDir {
		writeError(w, r, storage.ErrIsDir)
		return
	}

	c, found := callerFrom(r.Context())
	if !found {
		writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Not signed in", "")
		return
	}

	token := s.signer.Sign(p, c.user.ID)
	writeJSON(w, r, http.StatusOK, linkResponse{
		URL:       s.contentBase + "/d/" + token + "/" + url.PathEscape(p.Name()),
		ExpiresAt: time.Now().Add(content.DefaultTTL).UTC(),
	})
}

const maxBundlePaths = 1000

type bundleRequest struct {
	Paths []string `json:"paths"`
}

// handleBundleLink mints a short-lived link to a zip of several items.
func (s *Server) handleBundleLink(w http.ResponseWriter, r *http.Request) {
	c, found := callerFrom(r.Context())
	if !found {
		writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Not signed in", "")
		return
	}

	var body bundleRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Paths) == 0 {
		writeProblem(w, r, http.StatusBadRequest, CodeBadRequest, "Nothing selected", "Choose at least one item to download.")
		return
	}
	if len(body.Paths) > maxBundlePaths {
		writeProblem(w, r, http.StatusRequestEntityTooLarge, CodeTooLarge,
			"Too many items", "Select fewer items, or download the folder that contains them.")
		return
	}

	paths := make([]storage.Path, 0, len(body.Paths))
	for _, raw := range body.Paths {
		p, ok := parsePath(w, r, raw)
		if !ok {
			return
		}
		if _, err := s.fs.Stat(r.Context(), p); err != nil {
			writeError(w, r, err)
			return
		}
		paths = append(paths, p)
	}

	token := s.signer.SignBundle(paths, c.user.ID)
	writeJSON(w, r, http.StatusOK, linkResponse{
		URL:       s.contentBase + "/z/" + token + "/" + url.PathEscape(bundleName(paths)),
		ExpiresAt: time.Now().Add(content.DefaultTTL).UTC(),
	})
}

func bundleName(paths []storage.Path) string {
	if len(paths) == 1 {
		return paths[0].Name() + ".zip"
	}
	return "zefile.zip"
}

type textResponse struct {
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

// handleText returns a file's content as text for the preview.
func (s *Server) handleText(w http.ResponseWriter, r *http.Request) {
	p, ok := pathParam(w, r)
	if !ok {
		return
	}
	p = s.resolveReference(r, p)
	info, err := s.fs.Stat(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if info.IsDir {
		writeError(w, r, storage.ErrIsDir)
		return
	}

	file, err := s.fs.Open(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer file.Close()

	max := int(s.previewMaxBytes)
	buf, err := io.ReadAll(io.LimitReader(file, int64(max)+1))
	if err != nil {
		writeError(w, r, err)
		return
	}
	truncated := len(buf) > max
	if truncated {
		buf = buf[:max]
	}

	writeJSON(w, r, http.StatusOK, textResponse{Content: string(buf), Truncated: truncated})
}

type spaceResponse struct {
	Available uint64 `json:"available"`
	Total     uint64 `json:"total"`
	Reserve   uint64 `json:"reserve"`
	ReadOnly  bool   `json:"read_only"`

	Used  int64 `json:"used"`
	Quota int64 `json:"quota"`

	InstanceUsed  int64 `json:"instance_used"`
	InstanceTotal int64 `json:"instance_total"`
}

func (s *Server) handleSpace(w http.ResponseWriter, r *http.Request) {
	info, err := s.fs.Space(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	resp := spaceResponse{
		Available: info.Available,
		Total:     info.Total,
		Reserve:   info.Reserve,
		ReadOnly:  info.ReadOnly,
	}
	if s.quota != nil {
		snap, err := s.quota.Snapshot(r.Context())
		if err != nil {
			writeError(w, r, err)
			return
		}
		resp.InstanceUsed = snap.TotalBytes
		resp.InstanceTotal = s.quota.ServerQuota()
		if resp.InstanceTotal <= 0 {
			resp.InstanceTotal = int64(info.Total)
		}
		if c, ok := callerFrom(r.Context()); ok {
			resp.Used = snap.ByUser[c.user.ID]
			if u, err := s.auth.GetUser(r.Context(), c.user.ID); err == nil {
				resp.Quota = u.QuotaBytes
			}
		}
	}
	writeJSON(w, r, http.StatusOK, resp)
}

type configResponse struct {
	InlinePreview bool `json:"inline_preview"`

	ObjectStorage bool `json:"object_storage"`

	URLFetch bool `json:"url_fetch"`

	Version string `json:"version"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, configResponse{
		InlinePreview: !s.singleOrigin || s.objectStorage,
		ObjectStorage: s.objectStorage,
		URLFetch:      s.urlFetch,
		Version:       s.version,
	})
}

func pathParam(w http.ResponseWriter, r *http.Request) (storage.Path, bool) {
	raw := r.URL.Query().Get("path")
	if raw == "" {
		raw = "/"
	}
	return parsePath(w, r, raw)
}

func parsePath(w http.ResponseWriter, r *http.Request, raw string) (storage.Path, bool) {
	p, err := storage.ParsePath(raw)
	if err != nil {
		writeError(w, r, err)
		return storage.Path{}, false
	}
	return p, true
}

func toEntryResponse(info storage.FileInfo) entryResponse {
	return entryResponse{
		Path:    info.Path.String(),
		Name:    info.Name,
		Size:    info.Size,
		ModTime: info.ModTime.UTC(),
		IsDir:   info.IsDir,
		Symlink: info.Symlink,
	}
}
