package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"authz-service/internal/logctx"
	"authz-service/internal/model"

	"github.com/google/uuid"
)

// captureJSONLogs routes slog's default logger, with a base service field as
// internal/app sets it, to a buffer for the rest of the test, and returns a
// function that parses the lines written so far.
func captureJSONLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)).With("service", "authz-api"))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("log line is not JSON: %q", line)
			}
			out = append(out, m)
		}
		return out
	}
}

func accessLines(records []map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range records {
		if r["msg"] == "http request" {
			out = append(out, r)
		}
	}
	return out
}

// AC-2: a request without X-Request-ID gets a generated UUID v7, returned in
// the response header and logged on the access line (it used to be logged empty).
func TestAccessLog_GeneratedRequestID(t *testing.T) {
	logs := captureJSONLogs(t)
	ts := newTestServer(t)

	rec := ts.do(t, "GET", "/healthz", "")

	id := rec.Header().Get("X-Request-ID")
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.Version() != 7 {
		t.Fatalf("X-Request-ID = %q, want a UUID v7 (err %v)", id, err)
	}
	lines := accessLines(logs())
	if len(lines) != 1 {
		t.Fatalf("access lines = %d, want 1", len(lines))
	}
	if lines[0]["request_id"] != id {
		t.Errorf("logged request_id = %v, want the generated %q", lines[0]["request_id"], id)
	}
	if lines[0]["service"] != "authz-api" {
		t.Errorf("logged service = %v, want authz-api", lines[0]["service"])
	}
	if _, ok := lines[0]["user_id"]; ok {
		t.Errorf("healthz has no user, but the line has user_id %v", lines[0]["user_id"])
	}
	for _, k := range []string{"method", "path", "status", "duration_ms"} {
		if _, ok := lines[0][k]; !ok {
			t.Errorf("access line lacks %q: %v", k, lines[0])
		}
	}
}

func TestAccessLog_ForwardedRequestID(t *testing.T) {
	logs := captureJSONLogs(t)
	ts := newTestServer(t)

	rec := ts.do(t, "GET", "/healthz", "", "X-Request-ID", "client-req-42")

	if got := rec.Header().Get("X-Request-ID"); got != "client-req-42" {
		t.Fatalf("response X-Request-ID = %q, want the forwarded id", got)
	}
	lines := accessLines(logs())
	if len(lines) != 1 || lines[0]["request_id"] != "client-req-42" {
		t.Fatalf("access lines = %v, want request_id client-req-42", lines)
	}
}

// AC-2: handlers that parse a user id add user_id to the access line.
func TestAccessLog_UserID(t *testing.T) {
	tests := []struct {
		name, method, path, body string
	}{
		{"check", "GET", "/query/v1/check?user_id=" + userID + "&action=read&resource_type=document&resource_id=d1", ""},
		{"permissions", "GET", "/query/v1/permissions?user_id=" + userID, ""},
		{"resources", "GET", "/query/v1/resources?user_id=" + userID + "&action=read&resource_type=document", ""},
		{"filter", "POST", "/query/v1/resources/filter", `{"user_id":"` + userID + `","action":"read","resource_type":"document","candidate_ids":["d1"]}`},
		{"assign role", "POST", "/admin/v1/users/" + userID + "/roles",
			`{"role_name":"editor","resource_type":"document","resource_id":"d1","granted_by":"` + userID + `"}`},
		{"revoke role", "DELETE", "/admin/v1/users/" + userID + "/roles/editor/document/d1", ""},
		{"set override", "POST", "/admin/v1/users/" + userID + "/overrides",
			`{"action":"read","resource_type":"document","resource_id":"d1","effect":"DENY","set_by":"` + userID + `"}`},
		{"remove override", "DELETE", "/admin/v1/users/" + userID + "/overrides/read/document/d1", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureJSONLogs(t)
			ts := newTestServer(t)

			rec := ts.do(t, tt.method, tt.path, tt.body)
			if rec.Code >= 400 {
				t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
			}

			lines := accessLines(logs())
			if len(lines) != 1 {
				t.Fatalf("access lines = %d, want 1", len(lines))
			}
			if lines[0]["user_id"] != userID {
				t.Errorf("user_id = %v, want %s", lines[0]["user_id"], userID)
			}
			if id, _ := lines[0]["request_id"].(string); id == "" {
				t.Error("access line lacks request_id")
			}
		})
	}

	t.Run("invalid user id is not logged", func(t *testing.T) {
		logs := captureJSONLogs(t)
		ts := newTestServer(t)
		ts.do(t, "GET", "/query/v1/permissions?user_id=not-a-uuid", "")
		lines := accessLines(logs())
		if len(lines) != 1 {
			t.Fatalf("access lines = %d, want 1", len(lines))
		}
		if _, ok := lines[0]["user_id"]; ok {
			t.Errorf("an unparsed user id was logged: %v", lines[0]["user_id"])
		}
	})
}

// AC-2: entries written while serving a request (here the panic recovery)
// carry the request's request_id and user_id through the context logger.
func TestRequestLogger_InHandlerEntries(t *testing.T) {
	logs := captureJSONLogs(t)
	uid := uuid.MustParse(userID)
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = withUserID(r, uid)
		logctx.FromContext(r.Context()).Info("handler entry")
		panic("boom")
	})
	h := RequestIDMiddleware(LoggingMiddleware(RecoveryMiddleware(panicking)))
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Request-ID", "req-7")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	byMsg := map[string]map[string]any{}
	for _, r := range logs() {
		byMsg[r["msg"].(string)] = r
	}
	for _, msg := range []string{"handler entry", "http request"} {
		r, ok := byMsg[msg]
		if !ok {
			t.Fatalf("no %q entry in %v", msg, byMsg)
		}
		if r["request_id"] != "req-7" || r["user_id"] != userID || r["service"] != "authz-api" {
			t.Errorf("%q entry = %v, want request_id req-7, user_id and service", msg, r)
		}
	}
	// The recovery runs in the handler chain's own context, which has the
	// request_id (user_id was added to a derived request inside the handler).
	if r := byMsg["panic recovered"]; r == nil || r["request_id"] != "req-7" {
		t.Errorf("panic entry = %v, want request_id req-7", r)
	}
	if r := byMsg["http request"]; r["status"] != float64(http.StatusInternalServerError) {
		t.Errorf("access line status = %v, want 500", r["status"])
	}
}

// 007 F-007: GET /query/v1/permissions passes resource_type and resource_id
// through to the resolver's queries.
func TestPermissionsHandler_PassesFilters(t *testing.T) {
	tests := []struct {
		query string
		want  [2]string
	}{
		{"", [2]string{"", ""}},
		{"&resource_type=document", [2]string{"document", ""}},
		{"&resource_type=document&resource_id=d1", [2]string{"document", "d1"}},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			ts := newTestServer(t)
			rec := ts.do(t, "GET", "/query/v1/permissions?user_id="+userID+tt.query, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			want := [][2]string{tt.want, tt.want} // role grants and overrides
			if !reflect.DeepEqual(ts.rstore.listFilters, want) {
				t.Fatalf("store filters = %v, want %v", ts.rstore.listFilters, want)
			}
		})
	}
}

// 008 F-009: an unscoped grant is returned with "scope": {}.
func TestPermissionsHandler_UnscopedGrantHasEmptyScope(t *testing.T) {
	ts := newTestServer(t)
	ts.rstore.grants = []model.RoleGrant{{Action: "read", ResourceType: "document", ResourceID: "d1", RoleName: "editor"}}
	rec := ts.do(t, "GET", "/query/v1/permissions?user_id="+userID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"scope":{}`) || strings.Contains(rec.Body.String(), `"scope":null`) {
		t.Fatalf("body = %s, want \"scope\":{}", rec.Body.String())
	}
}

// AC-6: the filter endpoint sends the candidates to the store and keeps their order.
func TestFilterHandler_CandidatesAndOrder(t *testing.T) {
	ts := newTestServer(t)
	ts.rstore.resourceIDs = []string{"d3", "d1"} // DB order differs from the request
	rec := ts.do(t, "POST", "/query/v1/resources/filter",
		`{"user_id":"`+userID+`","action":"read","resource_type":"document","candidate_ids":["d1","d2","d3"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if len(ts.rstore.filterCandidates) != 1 || !slices.Equal(ts.rstore.filterCandidates[0], []string{"d1", "d2", "d3"}) {
		t.Fatalf("FilterResources candidates = %v, want [[d1 d2 d3]]", ts.rstore.filterCandidates)
	}
	got := decodeBody[map[string][]string](t, rec)["allowed_ids"]
	if !slices.Equal(got, []string{"d1", "d3"}) {
		t.Fatalf("allowed_ids = %v, want [d1 d3]", got)
	}
}

// AC-10 (007 F-009) and AC-11: the generated Swagger documents the required
// request fields and has no fixed host.
func TestSwagger_RequiredFieldsAndRelativeHost(t *testing.T) {
	raw, err := os.ReadFile("../../docs/swagger.json")
	if err != nil {
		t.Fatalf("read swagger.json: %v", err)
	}
	var spec struct {
		Host        *string `json:"host"`
		Definitions map[string]struct {
			Required []string `json:"required"`
		} `json:"definitions"`
		Paths map[string]map[string]struct {
			Parameters []struct {
				In     string `json:"in"`
				Schema struct {
					Ref string `json:"$ref"`
				} `json:"schema"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse swagger.json: %v", err)
	}
	if spec.Host != nil {
		t.Errorf("swagger host = %q, want none (relative)", *spec.Host)
	}

	wantRequired := map[string][]string{
		"CreateActionRequest":   {"action", "resource_type"},
		"CreateRoleRequest":     {"name"},
		"RolePermissionRequest": {"action", "resource_type"},
		"AssignUserRoleRequest": {"granted_by", "resource_id", "resource_type", "role_name"},
		"SetOverrideRequest":    {"action", "effect", "resource_id", "resource_type", "set_by"},
		"FilterRequest":         {"action", "resource_type", "user_id"},
	}
	for name, want := range wantRequired {
		def, ok := spec.Definitions["internal_api."+name]
		if !ok {
			t.Errorf("definition %s missing", name)
			continue
		}
		got := slices.Clone(def.Required)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s required = %v, want %v", name, got, want)
		}
	}

	bodyRef := map[string]string{
		"/admin/v1/actions POST":                   "CreateActionRequest",
		"/admin/v1/roles POST":                     "CreateRoleRequest",
		"/admin/v1/roles/{name}/permissions POST":  "RolePermissionRequest",
		"/admin/v1/users/{user_id}/roles POST":     "AssignUserRoleRequest",
		"/admin/v1/users/{user_id}/overrides POST": "SetOverrideRequest",
		"/query/v1/resources/filter POST":          "FilterRequest",
	}
	for route, def := range bodyRef {
		path, method, _ := strings.Cut(route, " ")
		op, ok := spec.Paths[path][strings.ToLower(method)]
		if !ok {
			t.Errorf("%s not documented", route)
			continue
		}
		found := false
		for _, p := range op.Parameters {
			if p.In == "body" && p.Schema.Ref == "#/definitions/internal_api."+def {
				found = true
			}
		}
		if !found {
			t.Errorf("%s body is not documented as %s", route, def)
		}
	}
}
