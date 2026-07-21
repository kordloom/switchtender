package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/org"
)

// createOrgRequest is the JSON body accepted by POST /orgs.
type createOrgRequest struct {
	// Name labels the organization. Required.
	Name string `json:"name"`
}

// listOrgsResponse wraps the organization list.
type listOrgsResponse struct {
	// Orgs is the ordered list.
	Orgs []*org.Org `json:"orgs"`
	// Count is the number returned.
	Count int `json:"count"`
}

// orgMemberRequest is the JSON body accepted by POST /orgs/{id}/members.
type orgMemberRequest struct {
	// UserID is the account to add to the organization. Required.
	UserID string `json:"user_id"`
	// Role is the member's authority, admin or member. Empty defaults to member.
	Role org.Role `json:"role,omitempty"`
}

// orgMembersResponse wraps an organization's members.
type orgMembersResponse struct {
	// Members is the list of members with their roles.
	Members []org.Member `json:"members"`
	// Count is the number returned.
	Count int `json:"count"`
}

// createOrgHandler stores a new organization.
func createOrgHandler(store org.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "organizations not enabled")
			return
		}
		var req createOrgRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" {
			respondError(w, log, http.StatusBadRequest, "name is required")
			return
		}
		o := &org.Org{ID: org.NewID(), Name: req.Name, CreatedAt: time.Now()}
		if err := store.Save(r.Context(), o); err != nil {
			log.Error("server: save org: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store organization")
			return
		}
		respondJSON(w, log, http.StatusCreated, o, wantsPretty(r))
	}
}

// listOrgsHandler returns all organizations.
func listOrgsHandler(store org.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "organizations not enabled")
			return
		}
		list, err := store.List(r.Context())
		if err != nil {
			log.Error("server: list orgs: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list organizations")
			return
		}
		respondJSON(w, log, http.StatusOK,
			listOrgsResponse{Orgs: list, Count: len(list)}, wantsPretty(r))
	}
}

// deleteOrgHandler removes an organization and its memberships.
func deleteOrgHandler(store org.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "organizations not enabled")
			return
		}
		err := store.Delete(r.Context(), r.PathValue("id"))
		if errors.Is(err, org.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "organization not found")
			return
		}
		if err != nil {
			log.Error("server: delete org: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not delete organization")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"deleted": r.PathValue("id")}, wantsPretty(r))
	}
}

// listOrgMembersHandler returns an organization's members and their roles.
func listOrgMembersHandler(store org.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "organizations not enabled")
			return
		}
		if _, err := store.Get(r.Context(), r.PathValue("id")); errors.Is(err, org.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "organization not found")
			return
		}
		members, err := store.Members(r.Context(), r.PathValue("id"))
		if err != nil {
			log.Error("server: list org members: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list members")
			return
		}
		respondJSON(w, log, http.StatusOK,
			orgMembersResponse{Members: members, Count: len(members)}, wantsPretty(r))
	}
}

// addOrgMemberHandler adds a user to an organization with a role, defaulting to member.
func addOrgMemberHandler(store org.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "organizations not enabled")
			return
		}
		id := r.PathValue("id")
		if _, err := store.Get(r.Context(), id); errors.Is(err, org.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "organization not found")
			return
		}
		var req orgMemberRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.UserID == "" {
			respondError(w, log, http.StatusBadRequest, "user_id is required")
			return
		}
		role := req.Role
		if role == "" {
			role = org.RoleMember
		}
		if !org.ValidRole(role) {
			respondError(w, log, http.StatusBadRequest, "role must be admin or member")
			return
		}
		if err := store.AddMember(r.Context(), id, req.UserID, role); err != nil {
			log.Error("server: add org member: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not add member")
			return
		}
		respondJSON(w, log, http.StatusCreated, org.Member{UserID: req.UserID, Role: role}, wantsPretty(r))
	}
}

// removeOrgMemberHandler removes a user from an organization.
func removeOrgMemberHandler(store org.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "organizations not enabled")
			return
		}
		id, userID := r.PathValue("id"), r.PathValue("userID")
		if err := store.RemoveMember(r.Context(), id, userID); err != nil {
			log.Error("server: remove org member: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not remove member")
			return
		}
		respondJSON(w, log, http.StatusOK,
			map[string]string{"org_id": id, "user_id": userID}, wantsPretty(r))
	}
}
