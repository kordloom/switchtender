package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/team"
)

// createTeamRequest is the JSON body accepted by POST /teams.
type createTeamRequest struct {
	// Name labels the team. Required.
	Name string `json:"name"`
}

// listTeamsResponse wraps the team list.
type listTeamsResponse struct {
	// Teams is the ordered list.
	Teams []*team.Team `json:"teams"`
	// Count is the number returned.
	Count int `json:"count"`
}

// teamMemberRequest is the JSON body accepted by POST /teams/{id}/members.
type teamMemberRequest struct {
	// UserID is the account to add to the team. Required.
	UserID string `json:"user_id"`
}

// membersResponse wraps a team's member user ids.
type membersResponse struct {
	// Members is the list of member user ids.
	Members []string `json:"members"`
	// Count is the number returned.
	Count int `json:"count"`
}

// createTeamHandler stores a new team.
func createTeamHandler(store team.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "teams not enabled")
			return
		}
		var req createTeamRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" {
			respondError(w, log, http.StatusBadRequest, "name is required")
			return
		}
		tm := &team.Team{ID: team.NewID(), Name: req.Name, CreatedAt: time.Now()}
		if err := store.Save(r.Context(), tm); err != nil {
			log.Error("server: save team: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store team")
			return
		}
		respondJSON(w, log, http.StatusCreated, tm, wantsPretty(r))
	}
}

// listTeamsHandler returns all teams.
func listTeamsHandler(store team.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "teams not enabled")
			return
		}
		list, err := store.List(r.Context())
		if err != nil {
			log.Error("server: list teams: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list teams")
			return
		}
		respondJSON(w, log, http.StatusOK,
			listTeamsResponse{Teams: list, Count: len(list)}, wantsPretty(r))
	}
}

// deleteTeamHandler removes a team and its memberships.
func deleteTeamHandler(store team.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "teams not enabled")
			return
		}
		err := store.Delete(r.Context(), r.PathValue("id"))
		if errors.Is(err, team.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "team not found")
			return
		}
		if err != nil {
			log.Error("server: delete team: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not delete team")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"deleted": r.PathValue("id")}, wantsPretty(r))
	}
}

// listTeamMembersHandler returns a team's member user ids.
func listTeamMembersHandler(store team.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "teams not enabled")
			return
		}
		if _, err := store.Get(r.Context(), r.PathValue("id")); errors.Is(err, team.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "team not found")
			return
		}
		members, err := store.Members(r.Context(), r.PathValue("id"))
		if err != nil {
			log.Error("server: list team members: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list members")
			return
		}
		respondJSON(w, log, http.StatusOK,
			membersResponse{Members: members, Count: len(members)}, wantsPretty(r))
	}
}

// addTeamMemberHandler adds a user to a team.
func addTeamMemberHandler(store team.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "teams not enabled")
			return
		}
		id := r.PathValue("id")
		if _, err := store.Get(r.Context(), id); errors.Is(err, team.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "team not found")
			return
		}
		var req teamMemberRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.UserID == "" {
			respondError(w, log, http.StatusBadRequest, "user_id is required")
			return
		}
		if err := store.AddMember(r.Context(), id, req.UserID); err != nil {
			log.Error("server: add team member: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not add member")
			return
		}
		respondJSON(w, log, http.StatusCreated,
			map[string]string{"team_id": id, "user_id": req.UserID}, wantsPretty(r))
	}
}

// removeTeamMemberHandler removes a user from a team.
func removeTeamMemberHandler(store team.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "teams not enabled")
			return
		}
		id, userID := r.PathValue("id"), r.PathValue("userID")
		if err := store.RemoveMember(r.Context(), id, userID); err != nil {
			log.Error("server: remove team member: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not remove member")
			return
		}
		respondJSON(w, log, http.StatusOK,
			map[string]string{"team_id": id, "user_id": userID}, wantsPretty(r))
	}
}
