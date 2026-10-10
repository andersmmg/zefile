package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/krishna2206/zefile/internal/storage"
	"github.com/krishna2206/zefile/internal/upload"
)

// The tus core protocol, on the application origin rather than the content one.
//
// The design document places chunk writes on the content host. That separation
// exists to stop a hostile file from executing where the session lives, which
// is a property of *serving* content, not of receiving it. Keeping uploads here
// means the browser sends them same-origin, with no preflight and no second
// credential to arrange, and costs nothing in isolation.
const tusVersion = "1.0.0"

// tusHeaders marks every response so a standard client recognises the server.
func tusHeaders(w http.ResponseWriter) {
	w.Header().Set("Tus-Resumable", tusVersion)
}

func (s *Server) handleUploadOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Tus-Version", tusVersion)
	w.Header().Set("Tus-Extension", "creation")
	w.Header().Set("Tus-Max-Size", strconv.FormatInt(upload.MaxUploadSize, 10))
	tusHeaders(w)
	writeJSON(w, r, http.StatusNoContent, nil)
}

// handleUploadCreate opens a session.
//
// Metadata travels in the tus header rather than in a JSON body, so an
// off-the-shelf client works unchanged. The keys are `path` and, optionally,
// `checksum`.
func (s *Server) handleUploadCreate(w http.ResponseWriter, r *http.Request) {
	tusHeaders(w)

	c, ok := callerFrom(r.Context())
	if !ok {
		writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Not signed in", "")
		return
	}

	size, err := strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, CodeBadRequest,
			"Missing length", "Upload-Length must carry the total size in bytes.")
		return
	}

	metadata := parseUploadMetadata(r.Header.Get("Upload-Metadata"))
	p, ok := parsePath(w, r, metadata["path"])
	if !ok {
		return
	}

	session, err := s.uploads.Create(r.Context(), c.user.ID, p, size, metadata["checksum"])
	if err != nil {
		writeUploadError(w, r, err)
		return
	}

	// The caller owns what they are about to upload, recorded now so an
	// interrupted transfer still leaves ownership consistent with the session.
	if err := s.acl.SetOwner(r.Context(), p, c.user.ID); err != nil {
		writeError(w, r, err)
		return
	}

	w.Header().Set("Location", "/api/v1/uploads/"+session.Token)
	w.Header().Set("Upload-Offset", "0")
	w.Header().Set("Upload-Expires", session.ExpiresAt.Format(http.TimeFormat))
	writeJSON(w, r, http.StatusCreated, uploadResponse(session))
}

// handleUploadOffset answers where to resume from. It is the first thing a
// client asks after a dropped connection.
func (s *Server) handleUploadOffset(w http.ResponseWriter, r *http.Request) {
	tusHeaders(w)
	// An offset must never be served from a cache: a stale one would have the
	// client resume at the wrong place and produce a corrupt file.
	w.Header().Set("Cache-Control", "no-store")

	session, err := s.uploads.Offset(r.Context(), r.PathValue("token"))
	if err != nil {
		writeUploadError(w, r, err)
		return
	}

	w.Header().Set("Upload-Offset", strconv.FormatInt(session.Offset, 10))
	w.Header().Set("Upload-Length", strconv.FormatInt(session.Size, 10))
	if session.Mode == upload.ModeDirect {
		w.Header().Set("Upload-Part-Size", strconv.FormatInt(session.PartSize, 10))
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleUploadWrite(w http.ResponseWriter, r *http.Request) {
	tusHeaders(w)

	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/offset+octet-stream") {
		writeProblem(w, r, http.StatusUnsupportedMediaType, CodeBadRequest,
			"Wrong content type", "Chunks must be sent as application/offset+octet-stream.")
		return
	}

	offset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, CodeBadRequest,
			"Missing offset", "Upload-Offset must carry the position these bytes belong at.")
		return
	}

	session, err := s.uploads.Write(r.Context(), r.PathValue("token"), offset, r.Body)
	if err != nil {
		// A write that failed part way still moved the offset forward, and the
		// client needs to know where to resume from — so the header is set even
		// on the error path.
		if session.Offset > 0 {
			w.Header().Set("Upload-Offset", strconv.FormatInt(session.Offset, 10))
		}
		writeUploadError(w, r, err)
		return
	}

	w.Header().Set("Upload-Offset", strconv.FormatInt(session.Offset, 10))
	if !session.Complete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.thumbAfterUpload(r.Context(), session)
	writeJSON(w, r, http.StatusOK, uploadResponse(session))
}

// handleUploadParts mints presigned part URLs for a direct session.
// The body names the 1-based part numbers: {"parts":[3,4,5,6]}.
func (s *Server) handleUploadParts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Parts []int `json:"parts"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	urls, err := s.uploads.PartURLs(r.Context(), r.PathValue("token"), body.Parts)
	if err != nil {
		writeUploadError(w, r, err)
		return
	}

	parts := make([]storage.PartURL, 0, len(urls))
	parts = append(parts, urls...)
	writeJSON(w, r, http.StatusOK, map[string]any{"parts": parts})
}

// handleUploadComplete publishes a direct session once storage holds
// every declared byte.
func (s *Server) handleUploadComplete(w http.ResponseWriter, r *http.Request) {
	session, err := s.uploads.Complete(r.Context(), r.PathValue("token"))
	if err != nil {
		writeUploadError(w, r, err)
		return
	}
	s.thumbAfterUpload(r.Context(), session)
	writeJSON(w, r, http.StatusOK, uploadResponse(session))
}

// thumbAfterUpload asks the thumbnail worker to depict a freshly stored
// file. Best-effort: a miss on the next tile view enqueues the same job.
func (s *Server) thumbAfterUpload(ctx context.Context, session upload.Session) {
	if s.thumbs == nil {
		return
	}
	u, err := s.auth.GetUser(ctx, session.UserID)
	if err != nil {
		return
	}
	s.thumbs.Request(ctx, session.Path, u.ID, u.IsAdmin)
}

func (s *Server) handleUploadCancel(w http.ResponseWriter, r *http.Request) {
	tusHeaders(w)
	if err := s.uploads.Cancel(r.Context(), r.PathValue("token")); err != nil {
		writeUploadError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusNoContent, nil)
}

type uploadStatus struct {
	Token     string `json:"token"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Offset    int64  `json:"offset"`
	Complete  bool   `json:"complete"`
	ExpiresAt string `json:"expires_at"`
	Mode      string `json:"mode"`
	PartSize  int64  `json:"part_size,omitempty"`
}

func uploadResponse(s upload.Session) uploadStatus {
	return uploadStatus{
		Token:     s.Token,
		Path:      s.Path.String(),
		Size:      s.Size,
		Offset:    s.Offset,
		Complete:  s.Complete,
		ExpiresAt: s.ExpiresAt.Format("2006-01-02T15:04:05Z"),
		Mode:      s.Mode,
		PartSize:  s.PartSize,
	}
}

// parseUploadMetadata decodes the tus metadata header: comma-separated pairs of
// a key and a base64 value.
func parseUploadMetadata(header string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(header, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(pair), " ")
		if !found || key == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
		if err != nil {
			continue // A malformed pair is ignored; a missing path is caught later.
		}
		out[key] = string(decoded)
	}
	return out
}

func writeUploadError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, upload.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, CodeNotFound,
			"No such upload", "This upload session is unknown, expired, or already finished.")
	case errors.Is(err, upload.ErrOffsetMismatch):
		// 409 is what tus specifies, and it is the one a client is written to
		// handle by re-reading the offset.
		writeProblem(w, r, http.StatusConflict, CodeConflict,
			"Offset mismatch", err.Error())
	case errors.Is(err, upload.ErrChecksumMismatch):
		writeProblem(w, r, http.StatusBadRequest, CodeBadRequest,
			"Checksum mismatch", "The assembled file does not match the declared checksum; nothing was written.")
	case errors.Is(err, upload.ErrSizeMismatch):
		writeProblem(w, r, http.StatusBadRequest, CodeBadRequest,
			"Size mismatch", err.Error())
	case errors.Is(err, upload.ErrTooLarge):
		writeProblem(w, r, http.StatusRequestEntityTooLarge, CodeTooLarge,
			"Too large", "The declared length is beyond what this instance accepts.")
	case errors.Is(err, upload.ErrWrongMode):
		writeProblem(w, r, http.StatusConflict, CodeConflict,
			"Wrong transfer mode", "This upload receives bytes directly into storage: PUT each part to its presigned URL, then complete the session.")
	case errors.Is(err, storage.ErrExist):
		writeProblem(w, r, http.StatusConflict, CodeConflict,
			"Already exists", "Something already exists at the destination.")
	default:
		writeError(w, r, err)
	}
}
