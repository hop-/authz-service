package model

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

type Effect string

const (
	EffectAllow Effect = "ALLOW"
	EffectDeny  Effect = "DENY"
)

var (
	ErrUnknownPermission = errors.New("unknown permission")
	ErrUnknownRole       = errors.New("unknown role")
)

// --- Error response envelope (used by all HTTP handlers) ---

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ErrorResponse struct {
	Error APIError `json:"error"`
}

// --- Domain types ---

type Permission struct {
	Action       string    `json:"action"`
	ResourceType string    `json:"resource_type"`
	Description  string    `json:"description"`
	CreatedAt    time.Time `json:"created_at"`
}

type Role struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

type RolePermission struct {
	RoleName     string    `json:"role_name"`
	Action       string    `json:"action"`
	ResourceType string    `json:"resource_type"`
	GrantedAt    time.Time `json:"granted_at"`
}

type UserRole struct {
	UserID       uuid.UUID         `json:"user_id"`
	RoleName     string            `json:"role_name"`
	ResourceType string            `json:"resource_type"`
	ResourceID   string            `json:"resource_id"`
	Scope        map[string]string `json:"scope"`
	GrantedBy    uuid.UUID         `json:"granted_by"`
	GrantedAt    time.Time         `json:"granted_at"`
	ExpiresAt    *time.Time        `json:"expires_at"`
}

type UserPermissionOverride struct {
	UserID       uuid.UUID         `json:"user_id"`
	Action       string            `json:"action"`
	ResourceType string            `json:"resource_type"`
	ResourceID   string            `json:"resource_id"`
	Scope        map[string]string `json:"scope"`
	Effect       Effect            `json:"effect"`
	Reason       string            `json:"reason"`
	SetBy        uuid.UUID         `json:"set_by"`
	CreatedAt    time.Time         `json:"created_at"`
	ExpiresAt    *time.Time        `json:"expires_at"`
}

// --- Request/Response types ---

type CheckRequest struct {
	UserID       uuid.UUID
	Action       string
	ResourceType string
	ResourceID   string
}

type CheckResult struct {
	Allowed bool   `json:"allowed"`
	Source  string `json:"source"`
}

type ResourcesRequest struct {
	UserID       uuid.UUID
	Action       string
	ResourceType string
	ScopeFilter  map[string]string // nil = no filter
}

type ResourcesResult struct {
	ResourceIDs []string `json:"resource_ids"`
}

type PermissionEntry struct {
	Action       string            `json:"action"`
	ResourceType string            `json:"resource_type"`
	ResourceID   string            `json:"resource_id"`
	Scope        map[string]string `json:"scope"`
	Source       string            `json:"source"`
}

// --- DLQ types ---

type DLQReason string

const (
	DLQReasonPermanent          DLQReason = "permanent"
	DLQReasonTransientExhausted DLQReason = "transient_exhausted"
)

type DLQFailure struct {
	Reason     DLQReason `json:"reason"`
	Error      string    `json:"error"`
	RetryCount int       `json:"retry_count"`
	WorkerID   string    `json:"worker_id"`
}

type DLQSource struct {
	Topic     string `json:"topic"`
	Partition int    `json:"partition"`
	Offset    int64  `json:"offset"`
}

type DLQMessage struct {
	DLQEventID         string     `json:"dlq_event_id"`
	DLQOccurredAt      time.Time  `json:"dlq_occurred_at"`
	Failure            DLQFailure `json:"failure"`
	Source             DLQSource  `json:"source"`
	OriginalEventID    string     `json:"original_event_id"`
	OriginalEventType  string     `json:"original_event_type"`
	OriginalOccurredAt time.Time  `json:"original_occurred_at"`
	OriginalPayload    []byte     `json:"original_payload"`
}
