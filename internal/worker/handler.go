package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"authz-service/internal/db"
	"authz-service/internal/model"
	"authz-service/internal/producer"
	"authz-service/internal/registry"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/segmentio/kafka-go"
)

// Retry policy, applied separately to the DB phase and to the follow-up phase.
const (
	maxRetries     = 5
	initialBackoff = 100 * time.Millisecond
)

// EventEnvelope is the common envelope for all Kafka events.
type EventEnvelope struct {
	Event      string    `json:"event"`
	EventID    string    `json:"event_id"`
	OccurredAt time.Time `json:"occurred_at"`
}

// store is the worker's view of the primary database.
type store interface {
	ApplyEvent(ctx context.Context, eventID uuid.UUID, eventType string, fn func(db.Writer) error) (bool, error)
	PermissionID(ctx context.Context, action, resourceType string) (int16, bool, error)
	RoleID(ctx context.Context, name string) (int16, bool, error)
}

// invalidator removes cached check results.
type invalidator interface {
	Invalidate(ctx context.Context, userID, resourceType, resourceID string) error
	FlushAll(ctx context.Context) error
}

// publisher sends events to the main topic and to the dead-letter topic.
type publisher interface {
	Publish(ctx context.Context, key string, payload interface{}) error
	PublishDLQ(ctx context.Context, originalEventID string, payload interface{}) error
}

type Handler struct {
	store    store
	cache    invalidator
	registry *registry.Registry
	producer publisher
	workerID string
	// sleep waits d between retries and returns ctx.Err() if ctx is done first.
	// Tests replace it so retries run instantly.
	sleep func(ctx context.Context, d time.Duration) error
}

func NewHandler(s store, c invalidator, reg *registry.Registry, p publisher, workerID string) *Handler {
	return &Handler{
		store:    s,
		cache:    c,
		registry: reg,
		producer: p,
		workerID: workerID,
		sleep:    sleepContext,
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// eventPlan is the prepared, validated form of one event.
type eventPlan struct {
	// apply runs inside store.ApplyEvent's transaction. nil means the event has no DB phase.
	apply func(ctx context.Context, w db.Writer) error
	// after runs the idempotent follow-ups (cache, registry, *.id.assigned publish).
	// It runs on both the applied and the duplicate outcome; applied tells which.
	after func(ctx context.Context, applied bool) error
}

// validationError marks an error caused by the event's content. Replaying the
// same payload can never succeed, so it is permanent.
type validationError struct{ err error }

func (e *validationError) Error() string { return e.err.Error() }
func (e *validationError) Unwrap() error { return e.err }

func invalid(format string, args ...any) error {
	return &validationError{err: fmt.Errorf(format, args...)}
}

// isPermanent reports whether err will fail again on replay of the same payload:
// decode/validation errors, and PostgreSQL data exceptions (SQLSTATE class 22)
// or integrity constraint violations (class 23). Everything else, class 42
// included, is transient: a deploy can fix it, after which replay is correct.
func isPermanent(err error) bool {
	var vErr *validationError
	if errors.As(err, &vErr) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && len(pgErr.Code) >= 2 {
		switch pgErr.Code[:2] {
		case "22", "23":
			return true
		}
	}
	return false
}

// Handle processes one message. It returns nil when the message is handled
// (applied, duplicate, or routed to the DLQ) and a non-nil error only when
// processing was aborted because ctx was cancelled; the caller must then not
// commit the offset.
func (h *Handler) Handle(ctx context.Context, msg kafka.Message) error {
	var env EventEnvelope
	if err := json.Unmarshal(msg.Value, &env); err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("decode envelope: %w", err), 0)
	}

	eventID, err := uuid.Parse(env.EventID)
	if err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("invalid event_id %q: %w", env.EventID, err), 0)
	}

	slog.Info("processing event",
		"event_type", env.Event,
		"event_id", env.EventID,
	)

	plan, err := h.prepare(env, msg.Value)
	if err != nil {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, err, 0)
	}

	applied := false
	if plan.apply != nil {
		retries, err := h.retry(ctx, env, func(ctx context.Context) error {
			ok, err := h.store.ApplyEvent(ctx, eventID, env.Event, func(w db.Writer) error {
				return plan.apply(ctx, w)
			})
			applied = ok
			return err
		})
		if err != nil {
			return h.fail(ctx, msg, env, err, retries)
		}
		if !applied {
			slog.Info("duplicate event, running follow-ups only", "event_id", env.EventID, "event_type", env.Event)
		}
	}

	if plan.after != nil {
		retries, err := h.retry(ctx, env, func(ctx context.Context) error {
			return plan.after(ctx, applied)
		})
		if err != nil {
			return h.fail(ctx, msg, env, err, retries)
		}
	}

	return nil
}

// retry runs op until it succeeds, fails permanently, ctx is done, or
// maxRetries retries are used. It returns the number of retries made.
func (h *Handler) retry(ctx context.Context, env EventEnvelope, op func(ctx context.Context) error) (int, error) {
	delay := initialBackoff
	for attempt := 0; ; attempt++ {
		err := op(ctx)
		if err == nil {
			return attempt, nil
		}
		if ctx.Err() != nil || isPermanent(err) || attempt == maxRetries {
			return attempt, err
		}

		slog.Warn("transient error, retrying",
			"event_id", env.EventID,
			"event_type", env.Event,
			"attempt", attempt+1,
			"error", err,
		)

		if err := h.sleep(ctx, delay); err != nil {
			return attempt, err
		}
		delay *= 2
	}
}

// fail routes a phase failure: abort without DLQ when ctx is done, otherwise
// DLQ as permanent or transient_exhausted.
func (h *Handler) fail(ctx context.Context, msg kafka.Message, env EventEnvelope, err error, retries int) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		slog.Warn("event processing aborted",
			"event_id", env.EventID,
			"event_type", env.Event,
			"error", err,
		)
		return ctxErr
	}
	if isPermanent(err) {
		return h.sendToDLQ(ctx, msg, env, model.DLQReasonPermanent, err, retries)
	}
	return h.sendToDLQ(ctx, msg, env, model.DLQReasonTransientExhausted, err, retries)
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
		dlqJSON, jsonErr := json.Marshal(dlqMsg)
		if jsonErr != nil {
			dlqJSON = []byte(fmt.Sprintf("<marshal failed: %v>", jsonErr))
		}
		slog.Error("failed to publish to DLQ",
			"error", err,
			"dlq_message", string(dlqJSON),
		)
	}

	slog.Warn("message sent to DLQ",
		"event_id", env.EventID,
		"event_type", env.Event,
		"dlq_reason", reason,
		"retry_count", retryCount,
		"original_error", originalErr.Error(),
	)

	return nil // DLQ-routed messages are considered handled
}

// --- Prepare: decode, validate, resolve registry ids ---

// prepare builds the plan for one event. Every error it returns is a
// validationError, so it is permanent.
func (h *Handler) prepare(env EventEnvelope, payload []byte) (eventPlan, error) {
	switch env.Event {
	case "role.assigned":
		return h.prepareRoleAssigned(env, payload)
	case "role.revoked":
		return h.prepareRoleRevoked(payload)
	case "override.set":
		return h.prepareOverrideSet(env, payload)
	case "override.removed":
		return h.prepareOverrideRemoved(payload)
	case "action.registered":
		return h.prepareActionRegistered(payload)
	case "action.removed":
		return h.prepareActionRemoved(payload)
	case "action.id.assigned":
		return h.prepareActionIDAssigned(payload)
	case "role.created":
		return h.prepareRoleCreated(payload)
	case "role.removed":
		return h.prepareRoleRemoved(payload)
	case "role.id.assigned":
		return h.prepareRoleIDAssigned(payload)
	case "role_permissions.assigned":
		return h.prepareRolePermAssigned(payload)
	case "role_permissions.removed":
		return h.prepareRolePermRemoved(payload)
	default:
		return eventPlan{}, invalid("unknown event type: %s", env.Event)
	}
}

func decodePayload(payload []byte, v any) error {
	if err := json.Unmarshal(payload, v); err != nil {
		return invalid("decode payload: %w", err)
	}
	return nil
}

func parseUUID(field, value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, invalid("invalid %s: %w", field, err)
	}
	return id, nil
}

func (h *Handler) resolveRole(name string) (int16, error) {
	id, ok := h.registry.ResolveRole(name)
	if !ok {
		return 0, invalid("unknown role: %s", name)
	}
	return id, nil
}

func (h *Handler) resolvePermission(action, resourceType string) (int16, error) {
	id, ok := h.registry.ResolvePermission(action, resourceType)
	if !ok {
		return 0, invalid("unknown permission: (%s, %s)", action, resourceType)
	}
	return id, nil
}

// invalidateAfter is the follow-up for per-user grant changes.
func (h *Handler) invalidateAfter(userID uuid.UUID, resourceType, resourceID string) func(context.Context, bool) error {
	return func(ctx context.Context, _ bool) error {
		if err := h.cache.Invalidate(ctx, userID.String(), resourceType, resourceID); err != nil {
			return fmt.Errorf("invalidate cache: %w", err)
		}
		return nil
	}
}

func (h *Handler) flushAll(ctx context.Context) error {
	if err := h.cache.FlushAll(ctx); err != nil {
		return fmt.Errorf("flush cache: %w", err)
	}
	return nil
}

func (h *Handler) prepareRoleAssigned(env EventEnvelope, payload []byte) (eventPlan, error) {
	var evt struct {
		UserID       string            `json:"user_id"`
		RoleName     string            `json:"role_name"`
		ResourceType string            `json:"resource_type"`
		ResourceID   string            `json:"resource_id"`
		Scope        map[string]string `json:"scope"`
		GrantedBy    string            `json:"granted_by"`
		ExpiresAt    *time.Time        `json:"expires_at"`
	}
	if err := decodePayload(payload, &evt); err != nil {
		return eventPlan{}, err
	}
	userID, err := parseUUID("user_id", evt.UserID)
	if err != nil {
		return eventPlan{}, err
	}
	roleID, err := h.resolveRole(evt.RoleName)
	if err != nil {
		return eventPlan{}, err
	}
	grantedBy, err := parseUUID("granted_by", evt.GrantedBy)
	if err != nil {
		return eventPlan{}, err
	}

	return eventPlan{
		apply: func(ctx context.Context, w db.Writer) error {
			return w.InsertUserRole(ctx, userID, roleID, evt.ResourceType, evt.ResourceID, evt.Scope, grantedBy, env.OccurredAt, evt.ExpiresAt)
		},
		after: h.invalidateAfter(userID, evt.ResourceType, evt.ResourceID),
	}, nil
}

func (h *Handler) prepareRoleRevoked(payload []byte) (eventPlan, error) {
	var evt struct {
		UserID       string `json:"user_id"`
		RoleName     string `json:"role_name"`
		ResourceType string `json:"resource_type"`
		ResourceID   string `json:"resource_id"`
	}
	if err := decodePayload(payload, &evt); err != nil {
		return eventPlan{}, err
	}
	userID, err := parseUUID("user_id", evt.UserID)
	if err != nil {
		return eventPlan{}, err
	}
	roleID, err := h.resolveRole(evt.RoleName)
	if err != nil {
		return eventPlan{}, err
	}

	return eventPlan{
		apply: func(ctx context.Context, w db.Writer) error {
			return w.DeleteUserRole(ctx, userID, roleID, evt.ResourceType, evt.ResourceID)
		},
		after: h.invalidateAfter(userID, evt.ResourceType, evt.ResourceID),
	}, nil
}

func (h *Handler) prepareOverrideSet(env EventEnvelope, payload []byte) (eventPlan, error) {
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
	if err := decodePayload(payload, &evt); err != nil {
		return eventPlan{}, err
	}
	userID, err := parseUUID("user_id", evt.UserID)
	if err != nil {
		return eventPlan{}, err
	}
	permID, err := h.resolvePermission(evt.Action, evt.ResourceType)
	if err != nil {
		return eventPlan{}, err
	}
	setBy, err := parseUUID("set_by", evt.SetBy)
	if err != nil {
		return eventPlan{}, err
	}

	return eventPlan{
		apply: func(ctx context.Context, w db.Writer) error {
			return w.UpsertOverride(ctx, userID, permID, evt.ResourceID, evt.Scope, model.Effect(evt.Effect), evt.Reason, setBy, env.OccurredAt, evt.ExpiresAt)
		},
		after: h.invalidateAfter(userID, evt.ResourceType, evt.ResourceID),
	}, nil
}

func (h *Handler) prepareOverrideRemoved(payload []byte) (eventPlan, error) {
	var evt struct {
		UserID       string `json:"user_id"`
		Action       string `json:"action"`
		ResourceType string `json:"resource_type"`
		ResourceID   string `json:"resource_id"`
	}
	if err := decodePayload(payload, &evt); err != nil {
		return eventPlan{}, err
	}
	userID, err := parseUUID("user_id", evt.UserID)
	if err != nil {
		return eventPlan{}, err
	}
	permID, err := h.resolvePermission(evt.Action, evt.ResourceType)
	if err != nil {
		return eventPlan{}, err
	}

	return eventPlan{
		apply: func(ctx context.Context, w db.Writer) error {
			return w.DeleteOverride(ctx, userID, permID, evt.ResourceID)
		},
		after: h.invalidateAfter(userID, evt.ResourceType, evt.ResourceID),
	}, nil
}

func (h *Handler) prepareActionRegistered(payload []byte) (eventPlan, error) {
	var evt struct {
		Action       string `json:"action"`
		ResourceType string `json:"resource_type"`
		Description  string `json:"description"`
	}
	if err := decodePayload(payload, &evt); err != nil {
		return eventPlan{}, err
	}

	var insertedID int16
	return eventPlan{
		apply: func(ctx context.Context, w db.Writer) error {
			id, err := w.InsertPermission(ctx, evt.Action, evt.ResourceType, evt.Description)
			if err != nil {
				return err
			}
			insertedID = id
			return nil
		},
		after: func(ctx context.Context, applied bool) error {
			id := insertedID
			if !applied {
				var found bool
				var err error
				id, found, err = h.lookupPermissionID(ctx, evt.Action, evt.ResourceType)
				if err != nil {
					return err
				}
				if !found {
					slog.Info("permission no longer exists, skipping follow-ups",
						"action", evt.Action, "resource_type", evt.ResourceType)
					return nil
				}
			}

			h.registry.AddPermission(evt.Action, evt.ResourceType, id)

			idEvent := producer.NewEventEnvelope("action.id.assigned")
			idEvent["action"] = evt.Action
			idEvent["resource_type"] = evt.ResourceType
			idEvent["id"] = id
			if err := h.producer.Publish(ctx, "registry", idEvent); err != nil {
				return fmt.Errorf("publish action.id.assigned: %w", err)
			}
			return nil
		},
	}, nil
}

// lookupPermissionID resolves a permission id from the registry, falling back to the primary DB.
func (h *Handler) lookupPermissionID(ctx context.Context, action, resourceType string) (int16, bool, error) {
	if id, ok := h.registry.ResolvePermission(action, resourceType); ok {
		return id, true, nil
	}
	id, found, err := h.store.PermissionID(ctx, action, resourceType)
	if err != nil {
		return 0, false, fmt.Errorf("lookup permission (%s, %s): %w", action, resourceType, err)
	}
	return id, found, nil
}

func (h *Handler) prepareActionRemoved(payload []byte) (eventPlan, error) {
	var evt struct {
		Action       string `json:"action"`
		ResourceType string `json:"resource_type"`
	}
	if err := decodePayload(payload, &evt); err != nil {
		return eventPlan{}, err
	}

	return eventPlan{
		apply: func(ctx context.Context, w db.Writer) error {
			return w.DeletePermission(ctx, evt.Action, evt.ResourceType)
		},
		after: func(ctx context.Context, _ bool) error {
			h.registry.RemovePermission(evt.Action, evt.ResourceType)
			return h.flushAll(ctx)
		},
	}, nil
}

func (h *Handler) prepareActionIDAssigned(payload []byte) (eventPlan, error) {
	var evt struct {
		Action       string `json:"action"`
		ResourceType string `json:"resource_type"`
		ID           int16  `json:"id"`
	}
	if err := decodePayload(payload, &evt); err != nil {
		return eventPlan{}, err
	}

	// No DB phase: the registry update is an idempotent no-op if already present.
	return eventPlan{
		after: func(context.Context, bool) error {
			h.registry.AddPermission(evt.Action, evt.ResourceType, evt.ID)
			return nil
		},
	}, nil
}

func (h *Handler) prepareRoleCreated(payload []byte) (eventPlan, error) {
	var evt struct {
		RoleName    string `json:"role_name"`
		Description string `json:"description"`
	}
	if err := decodePayload(payload, &evt); err != nil {
		return eventPlan{}, err
	}

	var insertedID int16
	return eventPlan{
		apply: func(ctx context.Context, w db.Writer) error {
			id, err := w.InsertRole(ctx, evt.RoleName, evt.Description)
			if err != nil {
				return err
			}
			insertedID = id
			return nil
		},
		after: func(ctx context.Context, applied bool) error {
			id := insertedID
			if !applied {
				var found bool
				var err error
				id, found, err = h.lookupRoleID(ctx, evt.RoleName)
				if err != nil {
					return err
				}
				if !found {
					slog.Info("role no longer exists, skipping follow-ups", "role_name", evt.RoleName)
					return nil
				}
			}

			h.registry.AddRole(evt.RoleName, id)

			idEvent := producer.NewEventEnvelope("role.id.assigned")
			idEvent["role_name"] = evt.RoleName
			idEvent["id"] = id
			if err := h.producer.Publish(ctx, "registry", idEvent); err != nil {
				return fmt.Errorf("publish role.id.assigned: %w", err)
			}
			return nil
		},
	}, nil
}

// lookupRoleID resolves a role id from the registry, falling back to the primary DB.
func (h *Handler) lookupRoleID(ctx context.Context, name string) (int16, bool, error) {
	if id, ok := h.registry.ResolveRole(name); ok {
		return id, true, nil
	}
	id, found, err := h.store.RoleID(ctx, name)
	if err != nil {
		return 0, false, fmt.Errorf("lookup role %s: %w", name, err)
	}
	return id, found, nil
}

func (h *Handler) prepareRoleRemoved(payload []byte) (eventPlan, error) {
	var evt struct {
		RoleName string `json:"role_name"`
	}
	if err := decodePayload(payload, &evt); err != nil {
		return eventPlan{}, err
	}

	return eventPlan{
		apply: func(ctx context.Context, w db.Writer) error {
			return w.DeleteRole(ctx, evt.RoleName)
		},
		after: func(ctx context.Context, _ bool) error {
			h.registry.RemoveRole(evt.RoleName)
			return h.flushAll(ctx)
		},
	}, nil
}

func (h *Handler) prepareRoleIDAssigned(payload []byte) (eventPlan, error) {
	var evt struct {
		RoleName string `json:"role_name"`
		ID       int16  `json:"id"`
	}
	if err := decodePayload(payload, &evt); err != nil {
		return eventPlan{}, err
	}

	// No DB phase: the registry update is an idempotent no-op if already present.
	return eventPlan{
		after: func(context.Context, bool) error {
			h.registry.AddRole(evt.RoleName, evt.ID)
			return nil
		},
	}, nil
}

type rolePermEvent struct {
	RoleName     string `json:"role_name"`
	Action       string `json:"action"`
	ResourceType string `json:"resource_type"`
}

func (h *Handler) decodeRolePerm(payload []byte) (roleID, permID int16, err error) {
	var evt rolePermEvent
	if err := decodePayload(payload, &evt); err != nil {
		return 0, 0, err
	}
	roleID, err = h.resolveRole(evt.RoleName)
	if err != nil {
		return 0, 0, err
	}
	permID, err = h.resolvePermission(evt.Action, evt.ResourceType)
	if err != nil {
		return 0, 0, err
	}
	return roleID, permID, nil
}

func (h *Handler) prepareRolePermAssigned(payload []byte) (eventPlan, error) {
	roleID, permID, err := h.decodeRolePerm(payload)
	if err != nil {
		return eventPlan{}, err
	}
	return eventPlan{
		apply: func(ctx context.Context, w db.Writer) error {
			return w.InsertRolePermission(ctx, roleID, permID)
		},
		after: func(ctx context.Context, _ bool) error { return h.flushAll(ctx) },
	}, nil
}

func (h *Handler) prepareRolePermRemoved(payload []byte) (eventPlan, error) {
	roleID, permID, err := h.decodeRolePerm(payload)
	if err != nil {
		return eventPlan{}, err
	}
	return eventPlan{
		apply: func(ctx context.Context, w db.Writer) error {
			return w.DeleteRolePermission(ctx, roleID, permID)
		},
		after: func(ctx context.Context, _ bool) error { return h.flushAll(ctx) },
	}, nil
}
