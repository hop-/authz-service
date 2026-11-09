package api

import (
	"context"
	"net/http"
	"time"

	"authz-service/internal/model"
	"authz-service/internal/producer"
	"authz-service/internal/registry"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// adminStore is the admin API's read-only view of the database.
type adminStore interface {
	ListAllPermissions(ctx context.Context) ([]model.Permission, error)
	ListAllRoles(ctx context.Context) ([]model.Role, error)
	ListRolePermissions(ctx context.Context, roleID int16) ([]model.Permission, error)
}

// publisher sends admin events to the main topic.
type publisher interface {
	Publish(ctx context.Context, key string, payload interface{}) error
}

type AdminHandler struct {
	db       adminStore
	registry *registry.Registry
	producer publisher
}

func NewAdminHandler(s adminStore, reg *registry.Registry, p publisher) *AdminHandler {
	return &AdminHandler{db: s, registry: reg, producer: p}
}

// parseActor parses a required actor UUID field (granted_by, set_by). An
// empty value fails to parse, so it is rejected too.
func parseActor(value string) (uuid.UUID, bool) {
	id, err := uuid.Parse(value)
	return id, err == nil
}

// inFuture reports whether an optional expires_at is unset or after now.
func inFuture(t *time.Time) bool {
	return t == nil || t.After(time.Now())
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
// @Param body body CreateActionRequest true "Action definition"
// @Success 202 "Accepted"
// @Failure 400 {object} model.ErrorResponse
// @Failure 401 {object} model.ErrorResponse
// @Failure 409 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/actions [post]
func (h *AdminHandler) CreateAction(w http.ResponseWriter, r *http.Request) {
	var body CreateActionRequest
	if !decodeJSON(w, r, &body) {
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
// @Param body body CreateRoleRequest true "Role definition"
// @Success 202 "Accepted"
// @Failure 400 {object} model.ErrorResponse
// @Failure 401 {object} model.ErrorResponse
// @Failure 409 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/roles [post]
func (h *AdminHandler) CreateRole(w http.ResponseWriter, r *http.Request) {
	var body CreateRoleRequest
	if !decodeJSON(w, r, &body) {
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
// @Param body body RolePermissionRequest true "Permission"
// @Success 202 "Accepted"
// @Failure 400 {object} model.ErrorResponse
// @Failure 401 {object} model.ErrorResponse
// @Failure 404 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/roles/{name}/permissions [post]
func (h *AdminHandler) AssignRolePermission(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	var body RolePermissionRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Action == "" || body.ResourceType == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "action and resource_type are required")
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
// @Description granted_by is required and must be a UUID; expires_at, if set, must be in the future.
// @Description Re-assigning an existing (user, role, resource_type, resource_id) is a no-op. To change scope or extend `expires_at`, revoke and assign again.
// @Tags admin-user-roles
// @Accept json
// @Security AdminToken
// @Param user_id path string true "User UUID"
// @Param body body AssignUserRoleRequest true "Role assignment"
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
	r = withUserID(r, userID)

	var body AssignUserRoleRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.ResourceType == "" || body.ResourceID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "resource_type and resource_id are required")
		return
	}
	grantedBy, ok := parseActor(body.GrantedBy)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "granted_by is required and must be a valid UUID")
		return
	}
	if !inFuture(body.ExpiresAt) {
		writeError(w, http.StatusBadRequest, "invalid_request", "expires_at must be in the future")
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
	event["granted_by"] = grantedBy.String()
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
// @Failure 404 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/users/{user_id}/roles/{role_name}/{resource_type}/{resource_id} [delete]
func (h *AdminHandler) RevokeUserRole(w http.ResponseWriter, r *http.Request) {
	userIDStr := chi.URLParam(r, "user_id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "user_id must be a valid UUID")
		return
	}
	r = withUserID(r, userID)

	roleName := chi.URLParam(r, "role_name")
	resourceType := chi.URLParam(r, "resource_type")
	resourceID := chi.URLParam(r, "resource_id")

	if _, ok := h.registry.ResolveRole(roleName); !ok {
		writeError(w, http.StatusNotFound, "unknown_role", "role "+roleName+" not found")
		return
	}

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
// @Description effect must be exactly ALLOW or DENY; set_by is required and must be a UUID; expires_at, if set, must be in the future.
// @Tags admin-overrides
// @Accept json
// @Security AdminToken
// @Param user_id path string true "User UUID"
// @Param body body SetOverrideRequest true "Override definition"
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
	r = withUserID(r, userID)

	var body SetOverrideRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.ResourceID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "resource_id is required")
		return
	}
	if effect := model.Effect(body.Effect); effect != model.EffectAllow && effect != model.EffectDeny {
		writeError(w, http.StatusBadRequest, "invalid_request", "effect must be ALLOW or DENY")
		return
	}
	setBy, ok := parseActor(body.SetBy)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "set_by is required and must be a valid UUID")
		return
	}
	if !inFuture(body.ExpiresAt) {
		writeError(w, http.StatusBadRequest, "invalid_request", "expires_at must be in the future")
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
	event["set_by"] = setBy.String()
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
// @Failure 404 {object} model.ErrorResponse
// @Failure 500 {object} model.ErrorResponse
// @Router /admin/v1/users/{user_id}/overrides/{action}/{resource_type}/{resource_id} [delete]
func (h *AdminHandler) RemoveOverride(w http.ResponseWriter, r *http.Request) {
	userIDStr := chi.URLParam(r, "user_id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "user_id must be a valid UUID")
		return
	}
	r = withUserID(r, userID)

	action := chi.URLParam(r, "action")
	resourceType := chi.URLParam(r, "resource_type")
	resourceID := chi.URLParam(r, "resource_id")

	if _, ok := h.registry.ResolvePermission(action, resourceType); !ok {
		writeError(w, http.StatusNotFound, "unknown_permission", "permission ("+action+", "+resourceType+") not found")
		return
	}

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
