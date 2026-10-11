package api

import (
	"net/http"
	"strconv"

	"github.com/krishna2206/zefile/internal/acl"
	"github.com/krishna2206/zefile/internal/audit"
)

type permSet struct {
	Read   bool `json:"read"`
	Write  bool `json:"write"`
	Delete bool `json:"delete"`
	Share  bool `json:"share"`
	Manage bool `json:"manage"`
}

func toPermSet(p acl.Perm) permSet {
	return permSet{
		Read:   p.Has(acl.PermRead),
		Write:  p.Has(acl.PermWrite),
		Delete: p.Has(acl.PermDelete),
		Share:  p.Has(acl.PermShare),
		Manage: p.Has(acl.PermManage),
	}
}

func (ps permSet) toPerm() acl.Perm {
	var p acl.Perm
	if ps.Read {
		p |= acl.PermRead
	}
	if ps.Write {
		p |= acl.PermWrite
	}
	if ps.Delete {
		p |= acl.PermDelete
	}
	if ps.Share {
		p |= acl.PermShare
	}
	if ps.Manage {
		p |= acl.PermManage
	}
	return p
}

func (s *Server) handleEffectivePermissions(w http.ResponseWriter, r *http.Request) {
	p, ok := pathParam(w, r)
	if !ok {
		return
	}
	perms, err := s.acl.EffectivePerms(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toPermSet(perms))
}

type userSummary struct {
	ID         int64  `json:"id"`
	Username   string `json:"username"`
	IsAdmin    bool   `json:"is_admin"`
	Disabled   bool   `json:"disabled"`
	QuotaBytes int64  `json:"quota_bytes"`
	UsedBytes  int64  `json:"used_bytes"`
}

type userListResponse struct {
	Users []userSummary `json:"users"`
}

// handleListUsers lists accounts for the admin user picker.
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	users, err := s.auth.Users(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	var byUser map[int64]int64
	if s.quota != nil {
		if snap, err := s.quota.Snapshot(r.Context()); err == nil {
			byUser = snap.ByUser
		}
	}
	out := make([]userSummary, 0, len(users))
	for _, u := range users {
		out = append(out, userSummary{
			ID: u.ID, Username: u.Username, IsAdmin: u.IsAdmin, Disabled: u.Disabled,
			QuotaBytes: u.QuotaBytes, UsedBytes: byUser[u.ID],
		})
	}
	writeJSON(w, r, http.StatusOK, userListResponse{Users: out})
}

type accessRuleResponse struct {
	ID          int64   `json:"id"`
	SubjectType string  `json:"subject_type"`
	SubjectID   int64   `json:"subject_id"`
	SubjectName string  `json:"subject_name"`
	Perms       permSet `json:"perms"`
	Recursive   bool    `json:"recursive"`
	Deny        bool    `json:"deny"`
}

type accessListResponse struct {
	Rules []accessRuleResponse `json:"rules"`
}

// handleListAccess returns the rules attached to one path.
func (s *Server) handleListAccess(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	p, ok := pathParam(w, r)
	if !ok {
		return
	}

	rules, err := s.acl.RulesAt(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}

	userNames := map[int64]string{}
	if users, err := s.auth.Users(r.Context()); err == nil {
		for _, u := range users {
			userNames[u.ID] = u.Username
		}
	}
	groupNames := map[int64]string{}
	if groups, err := s.acl.ListGroups(r.Context()); err == nil {
		for _, g := range groups {
			groupNames[g.ID] = g.Name
		}
	}

	out := make([]accessRuleResponse, 0, len(rules))
	for _, rule := range rules {
		name := ""
		switch rule.SubjectType {
		case acl.SubjectUser:
			name = userNames[rule.SubjectID]
		case acl.SubjectGroup:
			name = groupNames[rule.SubjectID]
		}
		out = append(out, accessRuleResponse{
			ID:          rule.ID,
			SubjectType: string(rule.SubjectType),
			SubjectID:   rule.SubjectID,
			SubjectName: name,
			Perms:       toPermSet(rule.Perms),
			Recursive:   rule.Recursive,
			Deny:        rule.Deny,
		})
	}
	writeJSON(w, r, http.StatusOK, accessListResponse{Rules: out})
}

type grantAccessRequest struct {
	SubjectType string  `json:"subject_type"`
	SubjectID   int64   `json:"subject_id"`
	Path        string  `json:"path"`
	Perms       permSet `json:"perms"`
	Recursive   bool    `json:"recursive"`
	Deny        bool    `json:"deny"`
}

func (s *Server) handleGrantAccess(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}

	var body grantAccessRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	p, ok := parsePath(w, r, body.Path)
	if !ok {
		return
	}
	perms := body.Perms.toPerm()
	if perms == acl.PermNone {
		writeProblem(w, r, http.StatusBadRequest, CodeBadRequest,
			"No permissions", "Choose at least one permission to grant.")
		return
	}

	subjectType := acl.SubjectUser
	switch body.SubjectType {
	case "", "user":
		if _, err := s.auth.GetUser(r.Context(), body.SubjectID); err != nil {
			writeUserError(w, r, err)
			return
		}
	case "group":
		subjectType = acl.SubjectGroup
		if exists, err := s.acl.GroupExists(r.Context(), body.SubjectID); err != nil {
			writeError(w, r, err)
			return
		} else if !exists {
			writeProblem(w, r, http.StatusNotFound, CodeNotFound, "No such group", "This group does not exist.")
			return
		}
	default:
		writeProblem(w, r, http.StatusBadRequest, CodeBadRequest, "Bad subject", "subject_type must be user or group.")
		return
	}

	rule, err := s.acl.Grant(r.Context(), acl.Rule{
		SubjectType: subjectType,
		SubjectID:   body.SubjectID,
		Path:        p,
		Perms:       perms,
		Recursive:   body.Recursive,
		Deny:        body.Deny,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	s.audit(r, audit.ActionAccessGranted, p.String(), map[string]any{
		"subject_type": string(subjectType), "subject_id": body.SubjectID, "perms": perms.String(),
	})
	writeJSON(w, r, http.StatusCreated, accessRuleResponse{
		ID:          rule.ID,
		SubjectType: string(rule.SubjectType),
		SubjectID:   rule.SubjectID,
		Perms:       toPermSet(rule.Perms),
		Recursive:   rule.Recursive,
		Deny:        rule.Deny,
	})
}

func (s *Server) handleRevokeAccess(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, CodeBadRequest, "Bad id", "The rule id must be a number.")
		return
	}
	if err := s.acl.Revoke(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	s.audit(r, audit.ActionAccessRevoked, "", map[string]any{"rule_id": id})
	writeJSON(w, r, http.StatusNoContent, nil)
}
