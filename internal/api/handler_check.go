package api

import (
	"errors"
	"net/http"

	"authz-service/internal/model"
	"authz-service/internal/resolver"

	"github.com/google/uuid"
)

func handleCheck(res *resolver.Resolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userIDStr := r.URL.Query().Get("user_id")
		action := r.URL.Query().Get("action")
		resourceType := r.URL.Query().Get("resource_type")
		resourceID := r.URL.Query().Get("resource_id")

		if userIDStr == "" || action == "" || resourceType == "" || resourceID == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "user_id, action, resource_type, and resource_id are required")
			return
		}

		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "user_id must be a valid UUID")
			return
		}

		req := model.CheckRequest{
			UserID:       userID,
			Action:       action,
			ResourceType: resourceType,
			ResourceID:   resourceID,
		}

		result, err := res.Check(r.Context(), req)
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
