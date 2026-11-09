package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"authz-service/internal/db"
	"authz-service/internal/logctx"
	"authz-service/internal/metrics"
	"authz-service/internal/model"
	"authz-service/internal/producer"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/segmentio/kafka-go"
)

// Retry policy, applied separately to the DB phase and to the follow-up phase.
const (
	maxRetries     = 5
	initialBackoff = 100 * time.Millisecond
)

// DLQ publish policy: retried until it succeeds or ctx is done, so a message
// is never committed without reaching the DLQ.
const (
	dlqInitialBackoff = 100 * time.Millisecond
	dlqMaxBackoff     = 30 * time.Second
)

// EventEnvelope is the common envelope for all Kafka events.
type EventEnvelope struct {
	Event      string    `json:"event"`
	EventID    string    `json:"event_id"`
	OccurredAt time.Time `json:"occurred_at"`
	// UserID is set on per-user events only. It is decoded for log
	// correlation; each event's own payload struct is what gets applied.
	UserID string `json:"user_id,omitempty"`
}

// eventLogger returns ctx's logger with the event's correlation fields:
// event_id and event_type, plus user_id for per-user events.
func eventLogger(ctx context.Context, env EventEnvelope) *slog.Logger {
	args := []any{"event_id", env.EventID, "event_type", env.Event}
	if env.UserID != "" {
		args = append(args, "user_id", env.UserID)
	}
	return logctx.FromContext(ctx).With(args...)
}

// messageLogger is eventLogger for a raw message. A message whose envelope
// doesn't decode gets empty event fields; Handle dead-letters it.
func messageLogger(ctx context.Context, msg kafka.Message) *slog.Logger {
	var env EventEnvelope
	// The decode error is Handle's to report; here it only leaves fields empty.
	_ = json.Unmarshal(msg.Value, &env)
	return eventLogger(ctx, env)
}

// store is the worker's view of the primary database. The worker keeps no
// in-memory registry: role and permission ids are resolved in each event's
// transaction (db.Writer), and PermissionID/RoleID serve the follow-ups of
// duplicate registrations.
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
	producer publisher
	workerID string
	// sleep waits d between retries and returns ctx.Err() if ctx is done first.
	// Tests replace it so retries run instantly.
	sleep func(ctx context.Context, d time.Duration) error
}

func NewHandler(s store, c invalidator, p publisher, workerID string) *Handler {
	return &Handler{
		store:    s,
		cache:    c,
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
	// after runs the idempotent follow-ups (cache, *.id.assigned publish).
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
// (applied, duplicate, or published to the DLQ) and a non-nil error only when
// processing was aborted because ctx was cancelled; the caller must then not
// commit the offset.
func (h *Handler) Handle(ctx context.Context, msg kafka.Message) error {
	start := time.Now()
	env, status, err := h.process(ctx, msg)
	if err != nil {
		return err // aborted: not counted
	}
	eventType := metrics.EventTypeLabel(env.Event)
	metrics.WorkerEventsProcessedTotal.WithLabelValues(eventType, status).Inc()
	metrics.WorkerProcessingDuration.WithLabelValues(eventType).Observe(time.Since(start).Seconds())
	return nil
}

// process is Handle without the metrics. On a nil error it also returns the
// decoded envelope and the outcome: StatusOK, StatusDuplicate or StatusDLQ.
func (h *Handler) process(ctx context.Context, msg kafka.Message) (EventEnvelope, string, error) {
	var env EventEnvelope
	envErr := json.Unmarshal(msg.Value, &env)
	// Every log entry about this event goes through its logger.
	ctx = logctx.WithContext(ctx, eventLogger(ctx, env))
	if envErr != nil {
		return env, metrics.StatusDLQ, h.deadLetter(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("decode envelope: %w", envErr), 0)
	}

	eventID, err := uuid.Parse(env.EventID)
	if err != nil {
		return env, metrics.StatusDLQ, h.deadLetter(ctx, msg, env, model.DLQReasonPermanent, fmt.Errorf("invalid event_id %q: %w", env.EventID, err), 0)
	}

	logctx.FromContext(ctx).Info("processing event")

	plan, err := h.prepare(env, msg.Value)
	if err != nil {
		return env, metrics.StatusDLQ, h.deadLetter(ctx, msg, env, model.DLQReasonPermanent, err, 0)
	}

	status := metrics.StatusOK
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
			return env, metrics.StatusDLQ, h.fail(ctx, msg, env, err, retries)
		}
		if !applied {
			status = metrics.StatusDuplicate
			logctx.FromContext(ctx).Info("duplicate event, running follow-ups only")
		}
	}

	if plan.after != nil {
		retries, err := h.retry(ctx, env, func(ctx context.Context) error {
			return plan.after(ctx, applied)
		})
		if err != nil {
			return env, metrics.StatusDLQ, h.fail(ctx, msg, env, err, retries)
		}
	}

	return env, status, nil
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

		logctx.FromContext(ctx).Warn("transient error, retrying",
			"attempt", attempt+1,
			"error", err,
		)

		if err := h.sleep(ctx, delay); err != nil {
			return attempt, err
		}
		delay *= 2
	}
}

// fail routes a phase failure to the DLQ as permanent or transient_exhausted.
func (h *Handler) fail(ctx context.Context, msg kafka.Message, env EventEnvelope, err error, retries int) error {
	reason := model.DLQReasonTransientExhausted
	if isPermanent(err) {
		reason = model.DLQReasonPermanent
	}
	return h.deadLetter(ctx, msg, env, reason, err, retries)
}

// deadLetter is the only way Handle reaches the DLQ. When ctx is already done
// it aborts without publishing, so the message is not committed and is
// redelivered; otherwise it publishes the DLQ message.
func (h *Handler) deadLetter(ctx context.Context, msg kafka.Message, env EventEnvelope, reason model.DLQReason, err error, retries int) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		logctx.FromContext(ctx).Warn("event processing aborted",
			"error", err,
		)
		return ctxErr
	}
	return h.sendToDLQ(ctx, msg, env, reason, err, retries)
}

// sendToDLQ publishes the DLQ message, retrying with capped backoff until the
// publish succeeds (nil: handled, the offset may be committed) or ctx is done
// (ctx error: aborted, the offset must not be committed). The partition stalls
// while the DLQ is unavailable, which is preferred over losing the event.
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

	log := logctx.FromContext(ctx)
	delay := dlqInitialBackoff
	for attempt := 1; ; attempt++ {
		err := h.producer.PublishDLQ(ctx, env.EventID, dlqMsg)
		if err == nil {
			break
		}
		// One entry per failed attempt. The first is an ERROR with the full
		// message, so it survives even if the process dies before the DLQ is
		// back; later attempts log a WARN.
		if attempt == 1 {
			dlqJSON, jsonErr := json.Marshal(dlqMsg)
			if jsonErr != nil {
				dlqJSON = []byte(fmt.Sprintf("<marshal failed: %v>", jsonErr))
			}
			log.Error("failed to publish to DLQ, retrying until it succeeds",
				"attempt", attempt,
				"backoff", delay,
				"error", err,
				"dlq_message", string(dlqJSON),
			)
		} else {
			log.Warn("DLQ publish failed, retrying",
				"attempt", attempt,
				"backoff", delay,
				"error", err,
			)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			log.Warn("DLQ publish aborted, offset not committed",
				"attempt", attempt,
				"error", ctxErr,
			)
			return ctxErr
		}
		if err := h.sleep(ctx, delay); err != nil {
			return err
		}
		delay = min(delay*2, dlqMaxBackoff)
	}

	log.Warn("message sent to DLQ",
		"dlq_reason", reason,
		"retry_count", retryCount,
		"original_error", originalErr.Error(),
	)

	return nil // published to the DLQ: handled
}

// --- Prepare: decode and validate ---

// prepare builds the plan for one event. Every error it returns is a
// validationError, so it is permanent. Role and permission ids are not
// resolved here: each apply resolves them inside the event's transaction.
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
	case "role.created":
		return h.prepareRoleCreated(payload)
	case "role.removed":
		return h.prepareRoleRemoved(payload)
	case "role_permissions.assigned":
		return h.prepareRolePermAssigned(payload)
	case "role_permissions.removed":
		return h.prepareRolePermRemoved(payload)
	case "action.id.assigned", "role.id.assigned":
		// For the API registries. The worker keeps no registry, so nothing to do.
		return eventPlan{}, nil
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

// roleIDIn resolves a role id inside the event's transaction. A role that
// doesn't exist is a validationError (permanent; the transaction rolls back);
// a failed lookup is transient.
func roleIDIn(ctx context.Context, w db.Writer, name string) (int16, error) {
	id, found, err := w.RoleID(ctx, name)
	if err != nil {
		return 0, fmt.Errorf("lookup role %s: %w", name, err)
	}
	if !found {
		return 0, invalid("unknown role: %s", name)
	}
	return id, nil
}

// permissionIDIn is roleIDIn for permissions.
func permissionIDIn(ctx context.Context, w db.Writer, action, resourceType string) (int16, error) {
	id, found, err := w.PermissionID(ctx, action, resourceType)
	if err != nil {
		return 0, fmt.Errorf("lookup permission (%s, %s): %w", action, resourceType, err)
	}
	if !found {
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
	grantedBy, err := parseUUID("granted_by", evt.GrantedBy)
	if err != nil {
		return eventPlan{}, err
	}

	return eventPlan{
		apply: func(ctx context.Context, w db.Writer) error {
			roleID, err := roleIDIn(ctx, w, evt.RoleName)
			if err != nil {
				return err
			}
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

	return eventPlan{
		apply: func(ctx context.Context, w db.Writer) error {
			roleID, err := roleIDIn(ctx, w, evt.RoleName)
			if err != nil {
				return err
			}
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
	setBy, err := parseUUID("set_by", evt.SetBy)
	if err != nil {
		return eventPlan{}, err
	}

	return eventPlan{
		apply: func(ctx context.Context, w db.Writer) error {
			permID, err := permissionIDIn(ctx, w, evt.Action, evt.ResourceType)
			if err != nil {
				return err
			}
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

	return eventPlan{
		apply: func(ctx context.Context, w db.Writer) error {
			permID, err := permissionIDIn(ctx, w, evt.Action, evt.ResourceType)
			if err != nil {
				return err
			}
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
				id, found, err = h.store.PermissionID(ctx, evt.Action, evt.ResourceType)
				if err != nil {
					return fmt.Errorf("lookup permission (%s, %s): %w", evt.Action, evt.ResourceType, err)
				}
				if !found {
					logctx.FromContext(ctx).Info("permission no longer exists, skipping follow-ups",
						"action", evt.Action, "resource_type", evt.ResourceType)
					return nil
				}
			}

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
		after: func(ctx context.Context, _ bool) error { return h.flushAll(ctx) },
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
				id, found, err = h.store.RoleID(ctx, evt.RoleName)
				if err != nil {
					return fmt.Errorf("lookup role %s: %w", evt.RoleName, err)
				}
				if !found {
					logctx.FromContext(ctx).Info("role no longer exists, skipping follow-ups", "role_name", evt.RoleName)
					return nil
				}
			}

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
		after: func(ctx context.Context, _ bool) error { return h.flushAll(ctx) },
	}, nil
}

type rolePermEvent struct {
	RoleName     string `json:"role_name"`
	Action       string `json:"action"`
	ResourceType string `json:"resource_type"`
}

// rolePermIDsIn resolves the role and permission of a role_permissions event
// inside its transaction.
func rolePermIDsIn(ctx context.Context, w db.Writer, evt rolePermEvent) (roleID, permID int16, err error) {
	roleID, err = roleIDIn(ctx, w, evt.RoleName)
	if err != nil {
		return 0, 0, err
	}
	permID, err = permissionIDIn(ctx, w, evt.Action, evt.ResourceType)
	if err != nil {
		return 0, 0, err
	}
	return roleID, permID, nil
}

func (h *Handler) prepareRolePermAssigned(payload []byte) (eventPlan, error) {
	var evt rolePermEvent
	if err := decodePayload(payload, &evt); err != nil {
		return eventPlan{}, err
	}
	return eventPlan{
		apply: func(ctx context.Context, w db.Writer) error {
			roleID, permID, err := rolePermIDsIn(ctx, w, evt)
			if err != nil {
				return err
			}
			return w.InsertRolePermission(ctx, roleID, permID)
		},
		after: func(ctx context.Context, _ bool) error { return h.flushAll(ctx) },
	}, nil
}

func (h *Handler) prepareRolePermRemoved(payload []byte) (eventPlan, error) {
	var evt rolePermEvent
	if err := decodePayload(payload, &evt); err != nil {
		return eventPlan{}, err
	}
	return eventPlan{
		apply: func(ctx context.Context, w db.Writer) error {
			roleID, permID, err := rolePermIDsIn(ctx, w, evt)
			if err != nil {
				return err
			}
			return w.DeleteRolePermission(ctx, roleID, permID)
		},
		after: func(ctx context.Context, _ bool) error { return h.flushAll(ctx) },
	}, nil
}
