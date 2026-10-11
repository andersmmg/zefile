package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/krishna2206/zefile/internal/audit"
	"github.com/krishna2206/zefile/internal/auth"
)

type invitationResponse struct {
	ID        int64     `json:"id"`
	Email     string    `json:"email,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`

	Token string `json:"token,omitempty"`
}

func toInvitationResponse(inv auth.Invitation) invitationResponse {
	return invitationResponse{
		ID:        inv.ID,
		Email:     inv.Email,
		CreatedAt: inv.CreatedAt.UTC(),
		ExpiresAt: inv.ExpiresAt.UTC(),
	}
}

type createInvitationRequest struct {
	Email string `json:"email"`
}

func (s *Server) handleInvitationCreate(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}

	var body createInvitationRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	token, inv, err := s.auth.Invite(r.Context(), c.user.ID, strings.TrimSpace(body.Email))
	if err != nil {
		writeError(w, r, err)
		return
	}
	out := toInvitationResponse(inv)
	out.Token = token
	s.audit(r, audit.ActionInvitationCreate, inv.Email, nil)
	writeJSON(w, r, http.StatusCreated, out)
}

type invitationListResponse struct {
	Invitations []invitationResponse `json:"invitations"`
}

func (s *Server) handleInvitationList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}

	invs, err := s.auth.ListInvitations(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	out := make([]invitationResponse, 0, len(invs))
	for _, inv := range invs {
		out = append(out, toInvitationResponse(inv))
	}
	writeJSON(w, r, http.StatusOK, invitationListResponse{Invitations: out})
}

func (s *Server) handleInvitationRevoke(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, CodeBadRequest, "Bad id", "The invitation id must be a number.")
		return
	}
	if err := s.auth.RevokeInvitation(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	s.audit(r, audit.ActionInvitationRevoke, "", map[string]any{"invitation_id": id})
	writeJSON(w, r, http.StatusNoContent, nil)
}

type invitationCheckResponse struct {
	Valid bool   `json:"valid"`
	Email string `json:"email,omitempty"`
}

func (s *Server) handleInvitationCheck(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	inv, err := s.auth.CheckInvitation(r.Context(), token)
	if err != nil {
		writeJSON(w, r, http.StatusOK, invitationCheckResponse{Valid: false})
		return
	}
	writeJSON(w, r, http.StatusOK, invitationCheckResponse{Valid: true, Email: inv.Email})
}

type acceptInvitationRequest struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleInvitationAccept(w http.ResponseWriter, r *http.Request) {
	var body acceptInvitationRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	user, err := s.auth.AcceptInvitation(r.Context(), body.Token, body.Username, body.Password)
	if err != nil {
		writeError(w, r, err)
		return
	}

	token, session, err := s.auth.CreateSession(r.Context(), user.ID, r.UserAgent(), clientIP(r))
	if err != nil {
		writeError(w, r, err)
		return
	}

	if err := s.ensureHome(r.Context(), user); err != nil {
		slog.ErrorContext(r.Context(), "provision home directory", "user", user.Username, "error", err)
	}

	http.SetCookie(w, auth.SessionCookie(token, session.ExpiresAt, s.secureCookies))
	s.auditAs(r, user.ID, audit.ActionUserJoined, user.Username, nil)

	codes, _ := s.auth.GenerateRecoveryCodes(r.Context(), user.ID)
	writeJSON(w, r, http.StatusCreated, sessionResponse{
		User:          toUserResponse(user),
		ExpiresAt:     session.ExpiresAt,
		Token:         token,
		RecoveryCodes: codes,
	})
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (caller, bool) {
	c, ok := callerFrom(r.Context())
	if !ok {
		writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Not signed in", "")
		return caller{}, false
	}
	if !c.user.IsAdmin {
		writeProblem(w, r, http.StatusForbidden, CodePermissionDenied,
			"Admins only", "This action is restricted to administrators.")
		return caller{}, false
	}
	return c, true
}
