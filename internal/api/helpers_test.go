package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"authz-service/internal/cache"
	"authz-service/internal/config"
	"authz-service/internal/db"
	"authz-service/internal/kafkatopics"
	"authz-service/internal/model"
	"authz-service/internal/producer"
	"authz-service/internal/registry"
	"authz-service/internal/resolver"

	"github.com/google/uuid"
)

// The production types must satisfy the api's consumer-side interfaces.
var (
	_ adminStore   = (*db.Queries)(nil)
	_ publisher    = (*producer.Producer)(nil)
	_ pinger       = (*db.Queries)(nil)
	_ pinger       = (*cache.Cache)(nil)
	_ pinger       = (*producer.Producer)(nil)
	_ topicChecker = kafkatopics.Checker{}
)

func TestMain(m *testing.M) {
	// The logging middleware writes one line per request; keep test output readable.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

const adminToken = "s3cret-token"

// --- Admin fakes ---

type fakeAdminStore struct {
	perms     []model.Permission
	roles     []model.Role
	rolePerms []model.Permission
	err       error
	roleIDs   []int16
}

func (s *fakeAdminStore) ListAllPermissions(context.Context) ([]model.Permission, error) {
	return s.perms, s.err
}

func (s *fakeAdminStore) ListAllRoles(context.Context) ([]model.Role, error) {
	return s.roles, s.err
}

func (s *fakeAdminStore) ListRolePermissions(_ context.Context, roleID int16) ([]model.Permission, error) {
	s.roleIDs = append(s.roleIDs, roleID)
	return s.rolePerms, s.err
}

type publishedEvent struct {
	key     string
	payload map[string]interface{}
}

type fakePublisher struct {
	events []publishedEvent
	err    error
}

func (p *fakePublisher) Publish(_ context.Context, key string, payload interface{}) error {
	m, _ := payload.(map[string]interface{})
	p.events = append(p.events, publishedEvent{key: key, payload: m})
	return p.err
}

// --- Resolver fakes ---

type fakeCheckCache struct{}

func (fakeCheckCache) Get(context.Context, model.CheckRequest) (*model.CheckResult, error) {
	return nil, nil
}

func (fakeCheckCache) Set(context.Context, model.CheckRequest, model.CheckResult, time.Duration) error {
	return nil
}

type fakeResolverStore struct {
	roleName    string
	resourceIDs []string
	grants      []model.RoleGrant
	overrides   []model.OverrideGrant
	err         error
}

func (s *fakeResolverStore) CheckOverride(context.Context, uuid.UUID, int16, string) (*model.Effect, *time.Time, error) {
	return nil, nil, s.err
}

func (s *fakeResolverStore) CheckRole(context.Context, uuid.UUID, int16, string, string) (string, *time.Time, error) {
	return s.roleName, nil, s.err
}

func (s *fakeResolverStore) Resources(context.Context, uuid.UUID, int16, string, map[string]string) ([]string, error) {
	return s.resourceIDs, s.err
}

func (s *fakeResolverStore) UserRoleGrants(context.Context, uuid.UUID, string, string) ([]model.RoleGrant, error) {
	return s.grants, s.err
}

func (s *fakeResolverStore) UserOverrides(context.Context, uuid.UUID, string, string) ([]model.OverrideGrant, error) {
	return s.overrides, s.err
}

// --- Health fakes ---

type fakePinger struct{ err error }

func (p fakePinger) Ping(context.Context) error { return p.err }

// fakeTopics reports the topics in missing as absent, or fails with err.
type fakeTopics struct {
	missing []string
	err     error
	asked   []string
}

func (f *fakeTopics) Missing(_ context.Context, topics ...string) ([]string, error) {
	f.asked = append(f.asked, topics...)
	return f.missing, f.err
}

const testTopic = "authz.permission-events"

// --- Server ---

type testServer struct {
	handler http.Handler
	reg     *registry.Registry
	store   *fakeAdminStore
	pub     *fakePublisher
	rstore  *fakeResolverStore
}

// newTestServer builds the real router in admin mode with registry entries:
// role "editor" (1) and permission (read, document) (2).
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	reg := registry.NewEmpty()
	reg.AddRole("editor", 1)
	reg.AddPermission("read", "document", 2)

	ts := &testServer{
		reg:    reg,
		store:  &fakeAdminStore{},
		pub:    &fakePublisher{},
		rstore: &fakeResolverStore{},
	}
	res := resolver.NewResolver(fakeCheckCache{}, ts.rstore, reg, time.Minute)
	admin := NewAdminHandler(ts.store, reg, ts.pub)
	health := &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: reg,
		Kafka: fakePinger{}, Topics: &fakeTopics{}, KafkaTopic: testTopic, AdminMode: true}
	cfg := &config.Config{AdminMode: true, AdminToken: adminToken}
	ts.handler = NewRouter(res, admin, health, cfg)
	return ts
}

// newQueryOnlyServer builds the router with admin mode off and no admin handler,
// as cmd/api does.
func newQueryOnlyServer(t *testing.T) *testServer {
	t.Helper()
	ts := newTestServer(t)
	res := resolver.NewResolver(fakeCheckCache{}, ts.rstore, ts.reg, time.Minute)
	health := &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: ts.reg,
		Topics: &fakeTopics{}, KafkaTopic: testTopic}
	ts.handler = NewRouter(res, nil, health, &config.Config{})
	return ts
}

// do sends a request; admin paths get the valid token unless headers override it.
func (ts *testServer) do(t *testing.T, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if strings.HasPrefix(path, "/admin/") {
		req.Header.Set("X-Admin-Token", adminToken)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
			continue
		}
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return v
}

// requireError checks status and the standard error envelope's code.
func requireError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) model.APIError {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, status, rec.Body.String())
	}
	resp := decodeBody[model.ErrorResponse](t, rec)
	if resp.Error.Code != code {
		t.Fatalf("error code = %q, want %q (message %q)", resp.Error.Code, code, resp.Error.Message)
	}
	return resp.Error
}

// oversizeJSON is a syntactically valid JSON object larger than maxBodyBytes.
func oversizeJSON(field string) string {
	return `{"` + field + `":"` + strings.Repeat("a", maxBodyBytes+1) + `"}`
}

var errBoom = errors.New("boom")
