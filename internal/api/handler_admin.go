package api

import (
	"encoding/json"
	"net/http"
	"time"

	"authz-service/internal/db"
	"authz-service/internal/model"
	"authz-service/internal/producer"
	"authz-service/internal/registry"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type AdminHandler struct {
	db       *db.Queries
	registry *registry.Registry
	producer *producer.Producer
}

func NewAdminHandler(q *db.Queries, reg *registry.Registry, p *producer.Producer) *AdminHandler {
	return &AdminHandler{db: q, registry: reg, producer: p}
}

// --- Action registry ---

// @Summary List all registered actions
// @Description Return all registered permission (action, resource_type) pairs.
// @Tags admin-actions
// @Produce json
// @Security AdminToken
// @Success 200 {array} model.Permission
// @Failure 401 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/actions [get]
func (h *AdminHandler) ListActions(w http.ResponseWriter, r *http.Request) {
	perms, err := h.db.ListAllPermissions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if perms == nil {
		perms = []model.Permission{}
	}
	writeJSON(w, http.StatusOK, perms)
}

// @Summary Register a new action
// @Description Create a new permission entry (action + resource_type). Async — returns 202.
// @Tags admin-actions
// @Accept json
// @Security AdminToken
// @Param body body object true "Action definition" SchemaExample({"action":"read","resource_type":"project","description":"Read access"})
// @Success 202 "Accepted"
// @Failure 400 {object} model.ErrorResponse
// @Failure 401 {object} model.ErrorResponse
// @Failure 409 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/actions [post]
func (h *AdminHandler) CreateAction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action       string `json:"action"`
		ResourceType string `json:"resource_type"`
		Description  string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if body.Action == "" || body.ResourceType == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "action and resource_type are required")
		return
	}

	if _, ok := h.registry.ResolvePermission(body.Action, body.ResourceType); ok {
		writeError(w, http.StatusConflict, "already_exists", "permission ("+body.Action+", "+body.ResourceType+") already exists")
		return
	}

	event := producer.NewEventEnvelope("action.registered")
	event["action"] = body.Action
	event["resource_type"] = body.ResourceType
	event["description"] = body.Description
	event["registered_by"] = uuid.Nil.String()

	if err := h.producer.Publish(r.Context(), "registry", event); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to publish event")
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// @Summary Remove an action
// @Description Delete a registered permission. Async — returns 202.
// @Tags admin-actions
// @Security AdminToken
// @Param action path string true "Action name"
// @Param resource_type path string true "Resource type"
// @Success 202 "Accepted"
// @Failure 401 {object} model.ErrorResponse
// @Failure 404 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/actions/{action}/{resource_type} [delete]
func (h *AdminHandler) DeleteAction(w http.ResponseWriter, r *http.Request) {
	action := chi.URLParam(r, "action")
	resourceType := chi.URLParam(r, "resource_type")

	if _, ok := h.registry.ResolvePermission(action, resourceType); !ok {
		writeError(w, http.StatusNotFound, "unknown_permission", "permission ("+action+", "+resourceType+") not found")
		return
	}

	event := producer.NewEventEnvelope("action.removed")
	event["action"] = action
	event["resource_type"] = resourceType
	event["removed_by"] = uuid.Nil.String()

	if err := h.producer.Publish(r.Context(), "registry", event); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to publish event")
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// --- Role management ---

// @Summary List all roles
// @Description Return all registered roles.
// @Tags admin-roles
// @Produce json
// @Security AdminToken
// @Success 200 {array} model.Role
// @Failure 401 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/roles [get]
func (h *AdminHandler) ListRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := h.db.ListAllRoles(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if roles == nil {
		roles = []model.Role{}
	}
	writeJSON(w, http.StatusOK, roles)
}

// @Summary Create a role
// @Description Create a new role. Async — returns 202.
// @Tags admin-roles
// @Accept json
// @Security AdminToken
// @Param body body object true "Role definition" SchemaExample({"name":"editor","description":"Can edit resources"})
// @Success 202 "Accepted"
// @Failure 400 {object} model.ErrorResponse
// @Failure 401 {object} model.ErrorResponse
// @Failure 409 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/roles [post]
func (h *AdminHandler) CreateRole(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}

	if _, ok := h.registry.ResolveRole(body.Name); ok {
		writeError(w, http.StatusConflict, "already_exists", "role "+body.Name+" already exists")
		return
	}

	event := producer.NewEventEnvelope("role.created")
	event["role_name"] = body.Name
	event["description"] = body.Description
	event["created_by"] = uuid.Nil.String()

	if err := h.producer.Publish(r.Context(), "registry", event); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to publish event")
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// @Summary List role permissions
// @Description Return all permissions assigned to a role.
// @Tags admin-roles
// @Produce json
// @Security AdminToken
// @Param name path string true "Role name"
// @Success 200 {array} model.Permission
// @Failure 401 {object} model.ErrorResponse
// @Failure 404 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/roles/{name}/permissions [get]
func (h *AdminHandler) ListRolePermissions(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	roleID, ok := h.registry.ResolveRole(name)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_role", "role "+name+" not found")
		return
	}

	perms, err := h.db.ListRolePermissions(r.Context(), roleID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if perms == nil {
		perms = []model.Permission{}
	}
	writeJSON(w, http.StatusOK, perms)
}

// @Summary Assign permission to role
// @Description Add a permission to a role. Async — returns 202.
// @Tags admin-roles
// @Accept json
// @Security AdminToken
// @Param name path string true "Role name"
// @Param body body object true "Permission" SchemaExample({"action":"read","resource_type":"project"})
// @Success 202 "Accepted"
// @Failure 400 {object} model.ErrorResponse
// @Failure 401 {object} model.ErrorResponse
// @Failure 404 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/roles/{name}/permissions [post]
func (h *AdminHandler) AssignRolePermission(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	var body struct {
		Action       string `json:"action"`
		ResourceType string `json:"resource_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}

	if _, ok := h.registry.ResolveRole(name); !ok {
		writeError(w, http.StatusNotFound, "unknown_role", "role "+name+" not found")
		return
	}
	if _, ok := h.registry.ResolvePermission(body.Action, body.ResourceType); !ok {
		writeError(w, http.StatusNotFound, "unknown_permission", "permission ("+body.Action+", "+body.ResourceType+") not found")
		return
	}

	event := producer.NewEventEnvelope("role_permissions.assigned")
	event["role_name"] = name
	event["action"] = body.Action
	event["resource_type"] = body.ResourceType
	event["assigned_by"] = uuid.Nil.String()

	if err := h.producer.Publish(r.Context(), "registry", event); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to publish event")
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// @Summary Remove permission from role
// @Description Remove a permission from a role. Async — returns 202.
// @Tags admin-roles
// @Security AdminToken
// @Param name path string true "Role name"
// @Param action path string true "Action name"
// @Param resource_type path string true "Resource type"
// @Success 202 "Accepted"
// @Failure 401 {object} model.ErrorResponse
// @Failure 404 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/roles/{name}/permissions/{action}/{resource_type} [delete]
func (h *AdminHandler) RemoveRolePermission(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	action := chi.URLParam(r, "action")
	resourceType := chi.URLParam(r, "resource_type")

	if _, ok := h.registry.ResolveRole(name); !ok {
		writeError(w, http.StatusNotFound, "unknown_role", "role "+name+" not found")
		return
	}
	if _, ok := h.registry.ResolvePermission(action, resourceType); !ok {
		writeError(w, http.StatusNotFound, "unknown_permission", "permission ("+action+", "+resourceType+") not found")
		return
	}

	event := producer.NewEventEnvelope("role_permissions.removed")
	event["role_name"] = name
	event["action"] = action
	event["resource_type"] = resourceType
	event["removed_by"] = uuid.Nil.String()

	if err := h.producer.Publish(r.Context(), "registry", event); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to publish event")
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// --- User role assignments ---

// @Summary Assign role to user
// @Description Assign a role to a user, scoped to a resource. Async — returns 202.
// @Tags admin-user-roles
// @Accept json
// @Security AdminToken
// @Param user_id path string true "User UUID"
// @Param body body object true "Role assignment" SchemaExample({"role_name":"editor","resource_type":"project","resource_id":"proj-1"})
// @Success 202 "Accepted"
// @Failure 400 {object} model.ErrorResponse
// @Failure 401 {object} model.ErrorResponse
// @Failure 404 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/users/{user_id}/roles [post]
func (h *AdminHandler) AssignUserRole(w http.ResponseWriter, r *http.Request) {
	userIDStr := chi.URLParam(r, "user_id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "user_id must be a valid UUID")
		return
	}

	var body struct {
		RoleName     string            `json:"role_name"`
		ResourceType string            `json:"resource_type"`
		ResourceID   string            `json:"resource_id"`
		Scope        map[string]string `json:"scope"`
		GrantedBy    string            `json:"granted_by"`
		ExpiresAt    *time.Time        `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}

	if _, ok := h.registry.ResolveRole(body.RoleName); !ok {
		writeError(w, http.StatusNotFound, "unknown_role", "role "+body.RoleName+" not found")
		return
	}

	event := producer.NewEventEnvelope("role.assigned")
	event["user_id"] = userID.String()
	event["role_name"] = body.RoleName
	event["resource_type"] = body.ResourceType
	event["resource_id"] = body.ResourceID
	event["scope"] = body.Scope
	event["granted_by"] = body.GrantedBy
	if body.ExpiresAt != nil {
		event["expires_at"] = body.ExpiresAt.Format(time.RFC3339)
	} else {
		event["expires_at"] = nil
	}

	if err := h.producer.Publish(r.Context(), userID.String(), event); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to publish event")
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// @Summary Revoke role from user
// @Description Revoke a user's role assignment for a specific resource. Async — returns 202.
// @Tags admin-user-roles
// @Security AdminToken
// @Param user_id path string true "User UUID"
// @Param role_name path string true "Role name"
// @Param resource_type path string true "Resource type"
// @Param resource_id path string true "Resource ID"
// @Success 202 "Accepted"
// @Failure 400 {object} model.ErrorResponse
// @Failure 401 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/users/{user_id}/roles/{role_name}/{resource_type}/{resource_id} [delete]
func (h *AdminHandler) RevokeUserRole(w http.ResponseWriter, r *http.Request) {
	userIDStr := chi.URLParam(r, "user_id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "user_id must be a valid UUID")
		return
	}

	roleName := chi.URLParam(r, "role_name")
	resourceType := chi.URLParam(r, "resource_type")
	resourceID := chi.URLParam(r, "resource_id")

	event := producer.NewEventEnvelope("role.revoked")
	event["user_id"] = userID.String()
	event["role_name"] = roleName
	event["resource_type"] = resourceType
	event["resource_id"] = resourceID
	event["revoked_by"] = uuid.Nil.String()

	if err := h.producer.Publish(r.Context(), userID.String(), event); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to publish event")
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// --- User permission overrides ---

// @Summary Set permission override
// @Description Set an ALLOW or DENY override for a user on a specific resource. Async — returns 202.
// @Tags admin-overrides
// @Accept json
// @Security AdminToken
// @Param user_id path string true "User UUID"
// @Param body body object true "Override definition" SchemaExample({"action":"write","resource_type":"project","resource_id":"proj-1","effect":"DENY","reason":"suspended"})
// @Success 202 "Accepted"
// @Failure 400 {object} model.ErrorResponse
// @Failure 401 {object} model.ErrorResponse
// @Failure 404 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/users/{user_id}/overrides [post]
func (h *AdminHandler) SetOverride(w http.ResponseWriter, r *http.Request) {
	userIDStr := chi.URLParam(r, "user_id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "user_id must be a valid UUID")
		return
	}

	var body struct {
		Action       string            `json:"action"`
		ResourceType string            `json:"resource_type"`
		ResourceID   string            `json:"resource_id"`
		Scope        map[string]string `json:"scope"`
		Effect       string            `json:"effect"`
		Reason       string            `json:"reason"`
		SetBy        string            `json:"set_by"`
		ExpiresAt    *time.Time        `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}

	if _, ok := h.registry.ResolvePermission(body.Action, body.ResourceType); !ok {
		writeError(w, http.StatusNotFound, "unknown_permission", "permission ("+body.Action+", "+body.ResourceType+") not found")
		return
	}

	event := producer.NewEventEnvelope("override.set")
	event["user_id"] = userID.String()
	event["action"] = body.Action
	event["resource_type"] = body.ResourceType
	event["resource_id"] = body.ResourceID
	event["scope"] = body.Scope
	event["effect"] = body.Effect
	event["reason"] = body.Reason
	event["set_by"] = body.SetBy
	if body.ExpiresAt != nil {
		event["expires_at"] = body.ExpiresAt.Format(time.RFC3339)
	} else {
		event["expires_at"] = nil
	}

	if err := h.producer.Publish(r.Context(), userID.String(), event); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to publish event")
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// @Summary Remove permission override
// @Description Remove a user's permission override for a specific resource. Async — returns 202.
// @Tags admin-overrides
// @Security AdminToken
// @Param user_id path string true "User UUID"
// @Param action path string true "Action name"
// @Param resource_type path string true "Resource type"
// @Param resource_id path string true "Resource ID"
// @Success 202 "Accepted"
// @Failure 400 {object} model.ErrorResponse
// @Failure 401 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/users/{user_id}/overrides/{action}/{resource_type}/{resource_id} [delete]
func (h *AdminHandler) RemoveOverride(w http.ResponseWriter, r *http.Request) {
	userIDStr := chi.URLParam(r, "user_id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "user_id must be a valid UUID")
		return
	}

	action := chi.URLParam(r, "action")
	resourceType := chi.URLParam(r, "resource_type")
	resourceID := chi.URLParam(r, "resource_id")

	event := producer.NewEventEnvelope("override.removed")
	event["user_id"] = userID.String()
	event["action"] = action
	event["resource_type"] = resourceType
	event["resource_id"] = resourceID
	event["removed_by"] = uuid.Nil.String()

	if err := h.producer.Publish(r.Context(), userID.String(), event); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to publish event")
		return
	}

	w.WriteHeader(http.StatusAccepted)
}
