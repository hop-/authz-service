package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"authz-service/internal/model"
	"authz-service/internal/resolver"

	"github.com/google/uuid"
)

func handleResources(res *resolver.Resolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userIDStr := r.URL.Query().Get("user_id")
		action := r.URL.Query().Get("action")
		resourceType := r.URL.Query().Get("resource_type")

		if userIDStr == "" || action == "" || resourceType == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "user_id, action, and resource_type are required")
			return
		}

		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "user_id must be a valid UUID")
			return
		}

		var scopeFilter map[string]string
		scopeFilterStr := r.URL.Query().Get("scope_filter")
		if scopeFilterStr != "" {
			if err := json.Unmarshal([]byte(scopeFilterStr), &scopeFilter); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", "scope_filter must be a flat JSON object")
				return
			}
		}

		req := model.ResourcesRequest{
			UserID:       userID,
			Action:       action,
			ResourceType: resourceType,
			ScopeFilter:  scopeFilter,
		}

		result, err := res.Resources(r.Context(), req)
		if err != nil {
			if errors.Is(err, model.ErrUnknownPermission) {
				writeError(w, http.StatusNotFound, "unknown_permission", "permission ("+action+", "+resourceType+") is not registered")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}

		writeJSON(w, http.StatusOK, result)
	}
}

func handleFilter(res *resolver.Resolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			UserID       string   `json:"user_id"`
			Action       string   `json:"action"`
			ResourceType string   `json:"resource_type"`
			CandidateIDs []string `json:"candidate_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
			return
		}

		if body.UserID == "" || body.Action == "" || body.ResourceType == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "user_id, action, and resource_type are required")
			return
		}

		userID, err := uuid.Parse(body.UserID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "user_id must be a valid UUID")
			return
		}

		if len(body.CandidateIDs) > 1000 {
			writeError(w, http.StatusBadRequest, "invalid_request", "candidate_ids exceeds limit of 1000")
			return
		}

		allowed, err := res.Filter(r.Context(), userID, body.Action, body.ResourceType, body.CandidateIDs)
		if err != nil {
			if errors.Is(err, model.ErrUnknownPermission) {
				writeError(w, http.StatusNotFound, "unknown_permission", "permission ("+body.Action+", "+body.ResourceType+") is not registered")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"allowed_ids": allowed,
		})
	}
}
