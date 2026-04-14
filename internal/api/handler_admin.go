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
