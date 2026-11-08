package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"authz-service/internal/cache"
	"authz-service/internal/db"
	"authz-service/internal/model"
	"authz-service/internal/producer"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
)

// The production types must satisfy the worker's consumer-side interfaces.
var (
	_ store         = (*db.Queries)(nil)
	_ invalidator   = (*cache.Cache)(nil)
	_ publisher     = (*producer.Producer)(nil)
	_ messageReader = (*kafka.Reader)(nil)
)

// --- Writer ---

type writerCall struct {
	method string
	args   []any
}

type fakeWriter struct {
	calls  []writerCall
	err    error // returned by every write
	permID int16 // returned by InsertPermission
	roleID int16 // returned by InsertRole

	// In-transaction natural-key lookups (not recorded in calls).
	roleIDs     map[string]int16
	permIDs     map[[2]string]int16
	lookupErr   error
	roleLookups []string
	permLookups [][2]string
}

func (w *fakeWriter) record(method string, args ...any) error {
	w.calls = append(w.calls, writerCall{method: method, args: args})
	return w.err
}

func (w *fakeWriter) InsertUserRole(_ context.Context, userID uuid.UUID, roleID int16, resourceType, resourceID string, scope map[string]string, grantedBy uuid.UUID, grantedAt time.Time, expiresAt *time.Time) error {
	return w.record("InsertUserRole", userID, roleID, resourceType, resourceID, scope, grantedBy, grantedAt.UTC(), expiresAt)
}

func (w *fakeWriter) DeleteUserRole(_ context.Context, userID uuid.UUID, roleID int16, resourceType, resourceID string) error {
	return w.record("DeleteUserRole", userID, roleID, resourceType, resourceID)
}

func (w *fakeWriter) UpsertOverride(_ context.Context, userID uuid.UUID, permissionID int16, resourceID string, scope map[string]string, effect model.Effect, reason string, setBy uuid.UUID, createdAt time.Time, expiresAt *time.Time) error {
	return w.record("UpsertOverride", userID, permissionID, resourceID, scope, effect, reason, setBy, createdAt.UTC(), expiresAt)
}

func (w *fakeWriter) DeleteOverride(_ context.Context, userID uuid.UUID, permissionID int16, resourceID string) error {
	return w.record("DeleteOverride", userID, permissionID, resourceID)
}

func (w *fakeWriter) InsertPermission(_ context.Context, action, resourceType, description string) (int16, error) {
	if err := w.record("InsertPermission", action, resourceType, description); err != nil {
		return 0, err
	}
	return w.permID, nil
}

func (w *fakeWriter) DeletePermission(_ context.Context, action, resourceType string) error {
	return w.record("DeletePermission", action, resourceType)
}

func (w *fakeWriter) InsertRole(_ context.Context, name, description string) (int16, error) {
	if err := w.record("InsertRole", name, description); err != nil {
		return 0, err
	}
	return w.roleID, nil
}

func (w *fakeWriter) DeleteRole(_ context.Context, name string) error {
	return w.record("DeleteRole", name)
}

func (w *fakeWriter) InsertRolePermission(_ context.Context, roleID, permissionID int16) error {
	return w.record("InsertRolePermission", roleID, permissionID)
}

func (w *fakeWriter) DeleteRolePermission(_ context.Context, roleID, permissionID int16) error {
	return w.record("DeleteRolePermission", roleID, permissionID)
}

func (w *fakeWriter) RoleID(_ context.Context, name string) (int16, bool, error) {
	w.roleLookups = append(w.roleLookups, name)
	if w.lookupErr != nil {
		return 0, false, w.lookupErr
	}
	id, ok := w.roleIDs[name]
	return id, ok, nil
}

func (w *fakeWriter) PermissionID(_ context.Context, action, resourceType string) (int16, bool, error) {
	key := [2]string{action, resourceType}
	w.permLookups = append(w.permLookups, key)
	if w.lookupErr != nil {
		return 0, false, w.lookupErr
	}
	id, ok := w.permIDs[key]
	return id, ok, nil
}

// --- Store ---

type applyCall struct {
	eventID   uuid.UUID
	eventType string
}

type fakeStore struct {
	writer *fakeWriter

	duplicate  bool    // ApplyEvent reports the event as already recorded
	applyErrs  []error // returned by successive ApplyEvent calls, then applyErr
	applyErr   error
	onApply    func() // runs at the start of every ApplyEvent call
	applyCalls []applyCall
	fnCalls    int

	permIDs     map[[2]string]int16
	roleIDs     map[string]int16
	lookupErr   error
	permLookups int
	roleLookups int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		writer: &fakeWriter{
			roleIDs: make(map[string]int16),
			permIDs: make(map[[2]string]int16),
		},
		permIDs: make(map[[2]string]int16),
		roleIDs: make(map[string]int16),
	}
}

func (s *fakeStore) ApplyEvent(_ context.Context, eventID uuid.UUID, eventType string, fn func(db.Writer) error) (bool, error) {
	s.applyCalls = append(s.applyCalls, applyCall{eventID, eventType})
	if s.onApply != nil {
		s.onApply()
	}
	if len(s.applyErrs) > 0 {
		err := s.applyErrs[0]
		s.applyErrs = s.applyErrs[1:]
		if err != nil {
			return false, err
		}
	} else if s.applyErr != nil {
		return false, s.applyErr
	}
	if s.duplicate {
		return false, nil
	}
	s.fnCalls++
	if err := fn(s.writer); err != nil {
		return false, err
	}
	return true, nil
}

func (s *fakeStore) PermissionID(_ context.Context, action, resourceType string) (int16, bool, error) {
	s.permLookups++
	if s.lookupErr != nil {
		return 0, false, s.lookupErr
	}
	id, ok := s.permIDs[[2]string{action, resourceType}]
	return id, ok, nil
}

func (s *fakeStore) RoleID(_ context.Context, name string) (int16, bool, error) {
	s.roleLookups++
	if s.lookupErr != nil {
		return 0, false, s.lookupErr
	}
	id, ok := s.roleIDs[name]
	return id, ok, nil
}

// --- Cache ---

type invalidation struct {
	userID, resourceType, resourceID string
}

type fakeCache struct {
	invalidations []invalidation
	flushes       int
	errs          []error // returned by successive calls (either method), then err
	err           error
}

func (c *fakeCache) next() error {
	if len(c.errs) > 0 {
		err := c.errs[0]
		c.errs = c.errs[1:]
		return err
	}
	return c.err
}

func (c *fakeCache) Invalidate(_ context.Context, userID, resourceType, resourceID string) error {
	c.invalidations = append(c.invalidations, invalidation{userID, resourceType, resourceID})
	return c.next()
}

func (c *fakeCache) FlushAll(_ context.Context) error {
	c.flushes++
	return c.next()
}

// --- Publisher ---

type published struct {
	key     string
	payload map[string]interface{}
}

type fakePublisher struct {
	published  []published
	publishErr error
	dlq        []model.DLQMessage // successfully published DLQ messages
	dlqKeys    []string
	dlqErrs    []error // returned by successive PublishDLQ calls, then dlqErr
	dlqErr     error
	dlqTries   []model.DLQMessage // every PublishDLQ call, failed or not
	onDLQ      func()             // runs at the start of every PublishDLQ call
}

func (p *fakePublisher) Publish(_ context.Context, key string, payload interface{}) error {
	m, _ := payload.(map[string]interface{})
	p.published = append(p.published, published{key: key, payload: m})
	return p.publishErr
}

func (p *fakePublisher) PublishDLQ(_ context.Context, originalEventID string, payload interface{}) error {
	if p.onDLQ != nil {
		p.onDLQ()
	}
	m, _ := payload.(model.DLQMessage)
	p.dlqTries = append(p.dlqTries, m)
	err := p.dlqErr
	if len(p.dlqErrs) > 0 {
		err = p.dlqErrs[0]
		p.dlqErrs = p.dlqErrs[1:]
	}
	if err != nil {
		return err
	}
	p.dlqKeys = append(p.dlqKeys, originalEventID)
	p.dlq = append(p.dlq, m)
	return nil
}

// --- Harness ---

type harness struct {
	h      *Handler
	store  *fakeStore
	cache  *fakeCache
	pub    *fakePublisher
	sleeps []time.Duration
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	hs := &harness{
		store: newFakeStore(),
		cache: &fakeCache{},
		pub:   &fakePublisher{},
	}
	hs.h = NewHandler(hs.store, hs.cache, hs.pub, "worker-1")
	hs.h.sleep = func(ctx context.Context, d time.Duration) error {
		hs.sleeps = append(hs.sleeps, d)
		return ctx.Err()
	}
	return hs
}

var (
	testEventID  = uuid.MustParse("0192f000-0000-7000-8000-000000000001")
	testUserID   = uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	testActorID  = uuid.MustParse("00000000-0000-0000-0000-0000000000bb")
	testOccurred = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
)

// message builds a Kafka message carrying an event with the given envelope fields
// plus payload fields.
func message(t *testing.T, eventType, eventID string, payload map[string]any) kafka.Message {
	t.Helper()
	body := map[string]any{
		"event":       eventType,
		"event_id":    eventID,
		"occurred_at": testOccurred.Format(time.RFC3339),
	}
	for k, v := range payload {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return kafka.Message{Topic: "authz.permission-events", Partition: 2, Offset: 41, Value: b}
}

func event(t *testing.T, eventType string, payload map[string]any) kafka.Message {
	t.Helper()
	return message(t, eventType, testEventID.String(), payload)
}

// handle runs Handle with a background context and requires the nil
// ("handled") result.
func (hs *harness) handle(t *testing.T, msg kafka.Message) {
	t.Helper()
	if err := hs.h.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle returned %v, want nil (handled)", err)
	}
}

func (hs *harness) requireNoDLQ(t *testing.T) {
	t.Helper()
	if len(hs.pub.dlq) != 0 {
		t.Fatalf("unexpected DLQ message(s): %+v", hs.pub.dlq[0].Failure)
	}
}

func (hs *harness) requireDLQ(t *testing.T, reason model.DLQReason, retryCount int) model.DLQMessage {
	t.Helper()
	if len(hs.pub.dlq) != 1 {
		t.Fatalf("got %d DLQ messages, want 1", len(hs.pub.dlq))
	}
	m := hs.pub.dlq[0]
	if m.Failure.Reason != reason || m.Failure.RetryCount != retryCount {
		t.Fatalf("DLQ failure = %s retry_count=%d (%s), want %s retry_count=%d",
			m.Failure.Reason, m.Failure.RetryCount, m.Failure.Error, reason, retryCount)
	}
	return m
}
