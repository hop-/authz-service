package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"authz-service/internal/cache"
	"authz-service/internal/db"
	"authz-service/internal/model"
	"authz-service/internal/producer"
	"authz-service/internal/registry"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
)

// EventEnvelope is the common envelope for all Kafka events.
type EventEnvelope struct {
	Event      string    `json:"event"`
	EventID    string    `json:"event_id"`
	OccurredAt time.Time `json:"occurred_at"`
}

type Handler struct {
	db       *db.Queries
	cache    *cache.Cache
	registry *registry.Registry
	producer *producer.Producer
	workerID string
}

func NewHandler(q *db.Queries, c *cache.Cache, reg *registry.Registry, p *producer.Producer, workerID string) *Handler {
	return &Handler{
		db:       q,
		cache:    c,
		registry: reg,
		producer: p,
		workerID: workerID,
	}
}

func (h *Handler) Handle(ctx context.Context, msg kafka.Message) error {
	var env EventEnvelope
	if err := json.Unmarshal(msg.Value, &env); err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, err, 0)
	}

	slog.Info("processing event",
		"event_type", env.Event,
		"event_id", env.EventID,
	)

	var err error
	switch env.Event {
	case "role.assigned":
		err = h.handleRoleAssigned(ctx, msg, env)
	case "role.revoked":
		err = h.handleRoleRevoked(ctx, msg, env)
	case "override.set":
		err = h.handleOverrideSet(ctx, msg, env)
	case "override.removed":
		err = h.handleOverrideRemoved(ctx, msg, env)
	case "action.registered":
		err = h.handleActionRegistered(ctx, msg, env)
	case "action.removed":
		err = h.handleActionRemoved(ctx, msg, env)
	case "action.id.assigned":
		err = h.handleActionIDAssigned(ctx, msg, env)
	case "role.created":
		err = h.handleRoleCreated(ctx, msg, env)
	case "role.removed":
		err = h.handleRoleRemoved(ctx, msg, env)
	case "role.id.assigned":
		err = h.handleRoleIDAssigned(ctx, msg, env)
	case "role_permissions.assigned":
		err = h.handleRolePermAssigned(ctx, msg, env)
	case "role_permissions.removed":
		err = h.handleRolePermRemoved(ctx, msg, env)
	default:
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("unknown event type: %s", env.Event), 0)
	}

	return err
}

func (h *Handler) withTransaction(ctx context.Context, msg kafka.Message, env EventEnvelope, fn func(ctx context.Context, tx interface {
	Exec(ctx context.Context, sql string, args ...any) (interface{ RowsAffected() int64 }, error)
}) error) error {
	// Use retry with exponential backoff for transient errors
	return h.withRetry(ctx, msg, env, func(ctx context.Context) error {
		eventID, err := uuid.Parse(env.EventID)
		if err != nil {
			return fmt.Errorf("invalid event_id: %w", err)
		}

		tx, err := h.db.BeginTx(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)

		// Deduplication: insert event_id into event_log
		isNew, err := h.db.InsertEventLog(ctx, tx, eventID, env.Event)
		if err != nil {
			return err
		}
		if !isNew {
			slog.Info("duplicate event, skipping", "event_id", env.EventID, "event_type", env.Event)
			return nil
		}

		// Execute the handler-specific logic
		if err := fn(ctx, nil); err != nil {
			return err
		}

		return tx.Commit(ctx)
	})
}

func (h *Handler) withRetry(ctx context.Context, msg kafka.Message, env EventEnvelope, fn func(ctx context.Context) error) error {
	maxRetries := 5
	delay := 100 * time.Millisecond

	for attempt := 0; attempt <= maxRetries; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}

		if attempt == maxRetries {
			return h.sendToDLQ(ctx, msg, env, model.DLQReasonTransientExhausted, err, attempt)
		}

		slog.Warn("transient error, retrying",
			"event_id", env.EventID,
			"event_type", env.Event,
			"attempt", attempt+1,
			"error", err,
		)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
			delay *= 2
		}
	}
	return nil
}

func (h *Handler) sendToDLQ(ctx context.Context, msg kafka.Message, env EventEnvelope, reason model.DLQReason, originalErr error, retryCount int) error {
	dlqMsg := model.DLQMessage{
		DLQEventID:    producer.NewEventID(),
		DLQOccurredAt: time.Now().UTC(),
		Failure: model.DLQFailure{
			Reason:     reason,
			Error:      originalErr.Error(),
			RetryCount: retryCount,
			WorkerID:   h.workerID,
		},
		Source: model.DLQSource{
			Topic:     msg.Topic,
			Partition: msg.Partition,
			Offset:    msg.Offset,
		},
		OriginalEventID:    env.EventID,
		OriginalEventType:  env.Event,
		OriginalOccurredAt: env.OccurredAt,
		OriginalPayload:    msg.Value,
	}

	if err := h.producer.PublishDLQ(ctx, env.EventID, dlqMsg); err != nil {
		// Best-effort: log the full DLQ message if publish fails
		dlqJSON, _ := json.Marshal(dlqMsg)
		slog.Error("failed to publish to DLQ",
			"error", err,
			"dlq_message", string(dlqJSON),
		)
	}

	slog.Warn("message sent to DLQ",
		"event_id", env.EventID,
		"event_type", env.Event,
		"dlq_reason", reason,
		"original_error", originalErr.Error(),
	)

	return nil // DLQ-routed messages are considered handled
}

// --- Per-event handlers ---

func (h *Handler) handleRoleAssigned(ctx context.Context, msg kafka.Message, env EventEnvelope) error {
	var evt struct {
		UserID       string            `json:"user_id"`
		RoleName     string            `json:"role_name"`
		ResourceType string            `json:"resource_type"`
		ResourceID   string            `json:"resource_id"`
		Scope        map[string]string `json:"scope"`
		GrantedBy    string            `json:"granted_by"`
		ExpiresAt    *time.Time        `json:"expires_at"`
	}
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, err, 0)
	}

	userID, err := uuid.Parse(evt.UserID)
	if err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("invalid user_id: %w", err), 0)
	}

	roleID, ok := h.registry.ResolveRole(evt.RoleName)
	if !ok {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("unknown role: %s", evt.RoleName), 0)
	}

	grantedBy, err := uuid.Parse(evt.GrantedBy)
	if err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("invalid granted_by: %w", err), 0)
	}

	return h.withRetry(ctx, msg, env, func(ctx context.Context) error {
		eventID, _ := uuid.Parse(env.EventID)
		tx, err := h.db.BeginTx(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)

		isNew, err := h.db.InsertEventLog(ctx, tx, eventID, env.Event)
		if err != nil {
			return err
		}
		if !isNew {
			return nil
		}

		if err := h.db.InsertUserRole(ctx, tx, userID, roleID, evt.ResourceType, evt.ResourceID, evt.Scope, grantedBy, env.OccurredAt, evt.ExpiresAt); err != nil {
			return err
		}

		if err := tx.Commit(ctx); err != nil {
			return err
		}

		return h.cache.Invalidate(ctx, userID.String(), evt.ResourceType, evt.ResourceID)
	})
}

func (h *Handler) handleRoleRevoked(ctx context.Context, msg kafka.Message, env EventEnvelope) error {
	var evt struct {
		UserID       string `json:"user_id"`
		RoleName     string `json:"role_name"`
		ResourceType string `json:"resource_type"`
		ResourceID   string `json:"resource_id"`
	}
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, err, 0)
	}

	userID, err := uuid.Parse(evt.UserID)
	if err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("invalid user_id: %w", err), 0)
	}

	roleID, ok := h.registry.ResolveRole(evt.RoleName)
	if !ok {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("unknown role: %s", evt.RoleName), 0)
	}

	return h.withRetry(ctx, msg, env, func(ctx context.Context) error {
		eventID, _ := uuid.Parse(env.EventID)
		tx, err := h.db.BeginTx(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)

		isNew, err := h.db.InsertEventLog(ctx, tx, eventID, env.Event)
		if err != nil {
			return err
		}
		if !isNew {
			return nil
		}

		if err := h.db.DeleteUserRole(ctx, tx, userID, roleID, evt.ResourceType, evt.ResourceID); err != nil {
			return err
		}

		if err := tx.Commit(ctx); err != nil {
			return err
		}

		return h.cache.Invalidate(ctx, userID.String(), evt.ResourceType, evt.ResourceID)
	})
}

func (h *Handler) handleOverrideSet(ctx context.Context, msg kafka.Message, env EventEnvelope) error {
	var evt struct {
		UserID       string            `json:"user_id"`
		Action       string            `json:"action"`
		ResourceType string            `json:"resource_type"`
		ResourceID   string            `json:"resource_id"`
		Scope        map[string]string `json:"scope"`
		Effect       string            `json:"effect"`
		Reason       string            `json:"reason"`
		SetBy        string            `json:"set_by"`
		ExpiresAt    *time.Time        `json:"expires_at"`
	}
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, err, 0)
	}

	userID, err := uuid.Parse(evt.UserID)
	if err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("invalid user_id: %w", err), 0)
	}

	permID, ok := h.registry.ResolvePermission(evt.Action, evt.ResourceType)
	if !ok {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("unknown permission: (%s, %s)", evt.Action, evt.ResourceType), 0)
	}

	setBy, err := uuid.Parse(evt.SetBy)
	if err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("invalid set_by: %w", err), 0)
	}

	return h.withRetry(ctx, msg, env, func(ctx context.Context) error {
		eventID, _ := uuid.Parse(env.EventID)
		tx, err := h.db.BeginTx(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)

		isNew, err := h.db.InsertEventLog(ctx, tx, eventID, env.Event)
		if err != nil {
			return err
		}
		if !isNew {
			return nil
		}

		if err := h.db.UpsertOverride(ctx, tx, userID, permID, evt.ResourceID, evt.Scope, model.Effect(evt.Effect), evt.Reason, setBy, env.OccurredAt, evt.ExpiresAt); err != nil {
			return err
		}

		if err := tx.Commit(ctx); err != nil {
			return err
		}

		return h.cache.Invalidate(ctx, userID.String(), evt.ResourceType, evt.ResourceID)
	})
}

func (h *Handler) handleOverrideRemoved(ctx context.Context, msg kafka.Message, env EventEnvelope) error {
	var evt struct {
		UserID       string `json:"user_id"`
		Action       string `json:"action"`
		ResourceType string `json:"resource_type"`
		ResourceID   string `json:"resource_id"`
	}
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, err, 0)
	}

	userID, err := uuid.Parse(evt.UserID)
	if err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("invalid user_id: %w", err), 0)
	}

	permID, ok := h.registry.ResolvePermission(evt.Action, evt.ResourceType)
	if !ok {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("unknown permission: (%s, %s)", evt.Action, evt.ResourceType), 0)
	}

	return h.withRetry(ctx, msg, env, func(ctx context.Context) error {
		eventID, _ := uuid.Parse(env.EventID)
		tx, err := h.db.BeginTx(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)

		isNew, err := h.db.InsertEventLog(ctx, tx, eventID, env.Event)
		if err != nil {
			return err
		}
		if !isNew {
			return nil
		}

		if err := h.db.DeleteOverride(ctx, tx, userID, permID, evt.ResourceID); err != nil {
			return err
		}

		if err := tx.Commit(ctx); err != nil {
			return err
		}

		return h.cache.Invalidate(ctx, userID.String(), evt.ResourceType, evt.ResourceID)
	})
}

func (h *Handler) handleActionRegistered(ctx context.Context, msg kafka.Message, env EventEnvelope) error {
	var evt struct {
		Action       string `json:"action"`
		ResourceType string `json:"resource_type"`
		Description  string `json:"description"`
	}
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, err, 0)
	}

	return h.withRetry(ctx, msg, env, func(ctx context.Context) error {
		eventID, _ := uuid.Parse(env.EventID)
		tx, err := h.db.BeginTx(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)

		isNew, err := h.db.InsertEventLog(ctx, tx, eventID, env.Event)
		if err != nil {
			return err
		}
		if !isNew {
			return nil
		}

		id, err := h.db.InsertPermission(ctx, tx, evt.Action, evt.ResourceType, evt.Description)
		if err != nil {
			return err
		}

		if err := tx.Commit(ctx); err != nil {
			return err
		}

		// Inline registry update
		h.registry.AddPermission(evt.Action, evt.ResourceType, id)

		// Publish action.id.assigned
		idEvent := producer.NewEventEnvelope("action.id.assigned")
		idEvent["action"] = evt.Action
		idEvent["resource_type"] = evt.ResourceType
		idEvent["id"] = id

		return h.producer.Publish(ctx, "registry", idEvent)
	})
}

func (h *Handler) handleActionRemoved(ctx context.Context, msg kafka.Message, env EventEnvelope) error {
	var evt struct {
		Action       string `json:"action"`
		ResourceType string `json:"resource_type"`
	}
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, err, 0)
	}

	return h.withRetry(ctx, msg, env, func(ctx context.Context) error {
		eventID, _ := uuid.Parse(env.EventID)
		tx, err := h.db.BeginTx(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)

		isNew, err := h.db.InsertEventLog(ctx, tx, eventID, env.Event)
		if err != nil {
			return err
		}
		if !isNew {
			return nil
		}

		if err := h.db.DeletePermission(ctx, tx, evt.Action, evt.ResourceType); err != nil {
			return err
		}

		if err := tx.Commit(ctx); err != nil {
			return err
		}

		h.registry.RemovePermission(evt.Action, evt.ResourceType)
		return h.cache.FlushAll(ctx)
	})
}

func (h *Handler) handleActionIDAssigned(ctx context.Context, msg kafka.Message, env EventEnvelope) error {
	var evt struct {
		Action       string `json:"action"`
		ResourceType string `json:"resource_type"`
		ID           int16  `json:"id"`
	}
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, err, 0)
	}

	// Idempotent no-op if already present from inline update
	h.registry.AddPermission(evt.Action, evt.ResourceType, evt.ID)
	return nil
}

func (h *Handler) handleRoleCreated(ctx context.Context, msg kafka.Message, env EventEnvelope) error {
	var evt struct {
		RoleName    string `json:"role_name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, err, 0)
	}

	return h.withRetry(ctx, msg, env, func(ctx context.Context) error {
		eventID, _ := uuid.Parse(env.EventID)
		tx, err := h.db.BeginTx(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)

		isNew, err := h.db.InsertEventLog(ctx, tx, eventID, env.Event)
		if err != nil {
			return err
		}
		if !isNew {
			return nil
		}

		id, err := h.db.InsertRole(ctx, tx, evt.RoleName, evt.Description)
		if err != nil {
			return err
		}

		if err := tx.Commit(ctx); err != nil {
			return err
		}

		// Inline registry update
		h.registry.AddRole(evt.RoleName, id)

		// Publish role.id.assigned
		idEvent := producer.NewEventEnvelope("role.id.assigned")
		idEvent["role_name"] = evt.RoleName
		idEvent["id"] = id

		return h.producer.Publish(ctx, "registry", idEvent)
	})
}

func (h *Handler) handleRoleRemoved(ctx context.Context, msg kafka.Message, env EventEnvelope) error {
	var evt struct {
		RoleName string `json:"role_name"`
	}
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, err, 0)
	}

	return h.withRetry(ctx, msg, env, func(ctx context.Context) error {
		eventID, _ := uuid.Parse(env.EventID)
		tx, err := h.db.BeginTx(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)

		isNew, err := h.db.InsertEventLog(ctx, tx, eventID, env.Event)
		if err != nil {
			return err
		}
		if !isNew {
			return nil
		}

		if err := h.db.DeleteRole(ctx, tx, evt.RoleName); err != nil {
			return err
		}

		if err := tx.Commit(ctx); err != nil {
			return err
		}

		h.registry.RemoveRole(evt.RoleName)
		return h.cache.FlushAll(ctx)
	})
}

func (h *Handler) handleRoleIDAssigned(ctx context.Context, msg kafka.Message, env EventEnvelope) error {
	var evt struct {
		RoleName string `json:"role_name"`
		ID       int16  `json:"id"`
	}
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, err, 0)
	}

	// Idempotent no-op if already present from inline update
	h.registry.AddRole(evt.RoleName, evt.ID)
	return nil
}

func (h *Handler) handleRolePermAssigned(ctx context.Context, msg kafka.Message, env EventEnvelope) error {
	var evt struct {
		RoleName     string `json:"role_name"`
		Action       string `json:"action"`
		ResourceType string `json:"resource_type"`
	}
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, err, 0)
	}

	roleID, ok := h.registry.ResolveRole(evt.RoleName)
	if !ok {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("unknown role: %s", evt.RoleName), 0)
	}

	permID, ok := h.registry.ResolvePermission(evt.Action, evt.ResourceType)
	if !ok {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("unknown permission: (%s, %s)", evt.Action, evt.ResourceType), 0)
	}

	return h.withRetry(ctx, msg, env, func(ctx context.Context) error {
		eventID, _ := uuid.Parse(env.EventID)
		tx, err := h.db.BeginTx(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)

		isNew, err := h.db.InsertEventLog(ctx, tx, eventID, env.Event)
		if err != nil {
			return err
		}
		if !isNew {
			return nil
		}

		if err := h.db.InsertRolePermission(ctx, tx, roleID, permID); err != nil {
			return err
		}

		if err := tx.Commit(ctx); err != nil {
			return err
		}

		return h.cache.FlushAll(ctx)
	})
}

func (h *Handler) handleRolePermRemoved(ctx context.Context, msg kafka.Message, env EventEnvelope) error {
	var evt struct {
		RoleName     string `json:"role_name"`
		Action       string `json:"action"`
		ResourceType string `json:"resource_type"`
	}
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, err, 0)
	}

	roleID, ok := h.registry.ResolveRole(evt.RoleName)
	if !ok {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("unknown role: %s", evt.RoleName), 0)
	}

	permID, ok := h.registry.ResolvePermission(evt.Action, evt.ResourceType)
	if !ok {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("unknown permission: (%s, %s)", evt.Action, evt.ResourceType), 0)
	}

	return h.withRetry(ctx, msg, env, func(ctx context.Context) error {
		eventID, _ := uuid.Parse(env.EventID)
		tx, err := h.db.BeginTx(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)

		isNew, err := h.db.InsertEventLog(ctx, tx, eventID, env.Event)
		if err != nil {
			return err
		}
		if !isNew {
			return nil
		}

		if err := h.db.DeleteRolePermission(ctx, tx, roleID, permID); err != nil {
			return err
		}

		if err := tx.Commit(ctx); err != nil {
			return err
		}

		return h.cache.FlushAll(ctx)
	})
}
