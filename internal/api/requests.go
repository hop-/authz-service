package api

import "time"

// Request bodies. The `binding:"required"` tags only document required
// fields in Swagger; the handlers validate them (400 invalid_request, or 404
// for an unregistered role or permission).

// CreateActionRequest is the body of POST /admin/v1/actions.
type CreateActionRequest struct {
	Action       string `json:"action" binding:"required" example:"read"`
	ResourceType string `json:"resource_type" binding:"required" example:"project"`
	Description  string `json:"description" example:"Read access"`
}

// CreateRoleRequest is the body of POST /admin/v1/roles.
type CreateRoleRequest struct {
	Name        string `json:"name" binding:"required" example:"editor"`
	Description string `json:"description" example:"Can edit resources"`
}

// RolePermissionRequest is the body of POST /admin/v1/roles/{name}/permissions.
type RolePermissionRequest struct {
	Action       string `json:"action" binding:"required" example:"read"`
	ResourceType string `json:"resource_type" binding:"required" example:"project"`
}

// AssignUserRoleRequest is the body of POST /admin/v1/users/{user_id}/roles.
type AssignUserRoleRequest struct {
	RoleName     string            `json:"role_name" binding:"required" example:"editor"`
	ResourceType string            `json:"resource_type" binding:"required" example:"project"`
	ResourceID   string            `json:"resource_id" binding:"required" example:"proj-1"`
	Scope        map[string]string `json:"scope"`
	// GrantedBy is the UUID of the actor granting the role.
	GrantedBy string `json:"granted_by" binding:"required" format:"uuid" example:"00000000-0000-0000-0000-000000000001"`
	// ExpiresAt, if set, must be in the future.
	ExpiresAt *time.Time `json:"expires_at"`
}

// SetOverrideRequest is the body of POST /admin/v1/users/{user_id}/overrides.
type SetOverrideRequest struct {
	Action       string            `json:"action" binding:"required" example:"write"`
	ResourceType string            `json:"resource_type" binding:"required" example:"project"`
	ResourceID   string            `json:"resource_id" binding:"required" example:"proj-1"`
	Scope        map[string]string `json:"scope"`
	Effect       string            `json:"effect" binding:"required" enums:"ALLOW,DENY" example:"DENY"`
	Reason       string            `json:"reason" example:"suspended"`
	// SetBy is the UUID of the actor setting the override.
	SetBy string `json:"set_by" binding:"required" format:"uuid" example:"00000000-0000-0000-0000-000000000001"`
	// ExpiresAt, if set, must be in the future.
	ExpiresAt *time.Time `json:"expires_at"`
}

// FilterRequest is the body of POST /query/v1/resources/filter.
type FilterRequest struct {
	UserID       string `json:"user_id" binding:"required" format:"uuid" example:"00000000-0000-0000-0000-0000000000aa"`
	Action       string `json:"action" binding:"required" example:"read"`
	ResourceType string `json:"resource_type" binding:"required" example:"project"`
	// CandidateIDs holds at most 1000 ids.
	CandidateIDs []string `json:"candidate_ids" example:"id1,id2"`
}
