package api

import (
	"net/http"

	"authz-service/internal/model"
	"authz-service/internal/resolver"

	"github.com/google/uuid"
)

// @Summary List effective permissions
// @Description List the effective permissions for a user, optionally filtered by resource type and resource ID.
// @Tags query
// @Produce json
// @Param user_id query string true "User UUID"
// @Param resource_type query string false "Filter by resource type"
// @Param resource_id query string false "Filter by resource ID"
// @Success 200 {object} map[string][]model.PermissionEntry
// @Failure 400 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /query/v1/permissions [get]
func handlePermissions(res *resolver.Resolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userIDStr := r.URL.Query().Get("user_id")
		if userIDStr == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "user_id is required")
			return
		}

		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "user_id must be a valid UUID")
			return
		}

		resourceType := r.URL.Query().Get("resource_type")
		resourceID := r.URL.Query().Get("resource_id")

		entries, err := res.List(r.Context(), userID, resourceType, resourceID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}

		if entries == nil {
			entries = []model.PermissionEntry{}
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"permissions": entries,
		})
	}
}
