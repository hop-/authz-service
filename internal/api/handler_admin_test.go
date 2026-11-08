package api

import (
	"net/http"
	"testing"
	"time"

	"authz-service/internal/model"
)

const (
	userID  = "00000000-0000-0000-0000-0000000000aa"
	actorID = "00000000-0000-0000-0000-0000000000bb"
)

var (
	future = time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	past   = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
)

type adminCase struct {
	name   string
	method string
	path   string
	body   string
	status int
	code   string // error code when status is 4xx/5xx
	// event and key of the single publish expected for a 202
	event string
	key   string
	// fields expected in the published event
	fields map[string]any
}

func runAdminCases(t *testing.T, cases []adminCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t)
			rec := ts.do(t, tc.method, tc.path, tc.body)

			if tc.status != http.StatusAccepted {
				requireError(t, rec, tc.status, tc.code)
				if len(ts.pub.events) != 0 {
					t.Fatalf("rejected request published %d event(s): %+v", len(ts.pub.events), ts.pub.events)
				}
				return
			}

			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
			}
			if len(ts.pub.events) != 1 {
				t.Fatalf("published %d events, want exactly 1", len(ts.pub.events))
			}
			ev := ts.pub.events[0]
			if ev.payload["event"] != tc.event {
				t.Errorf("event = %v, want %s", ev.payload["event"], tc.event)
			}
			if ev.key != tc.key {
				t.Errorf("partition key = %q, want %q", ev.key, tc.key)
			}
			for k, v := range tc.fields {
				if ev.payload[k] != v {
					t.Errorf("event %s = %#v, want %#v", k, ev.payload[k], v)
				}
			}
		})
	}
}

func TestAdmin_Actions(t *testing.T) {
	runAdminCases(t, []adminCase{
		{name: "create", method: "POST", path: "/admin/v1/actions",
			body:   `{"action":"write","resource_type":"document","description":"Write"}`,
			status: http.StatusAccepted, event: "action.registered", key: "registry",
			fields: map[string]any{"action": "write", "resource_type": "document", "description": "Write"}},
		{name: "create missing action", method: "POST", path: "/admin/v1/actions",
			body: `{"resource_type":"document"}`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "create missing resource_type", method: "POST", path: "/admin/v1/actions",
			body: `{"action":"write"}`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "create existing", method: "POST", path: "/admin/v1/actions",
			body: `{"action":"read","resource_type":"document"}`, status: http.StatusConflict, code: "already_exists"},
		{name: "create invalid JSON", method: "POST", path: "/admin/v1/actions",
			body: `{"action":`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "delete", method: "DELETE", path: "/admin/v1/actions/read/document",
			status: http.StatusAccepted, event: "action.removed", key: "registry",
			fields: map[string]any{"action": "read", "resource_type": "document"}},
		{name: "delete unknown", method: "DELETE", path: "/admin/v1/actions/fly/document",
			status: http.StatusNotFound, code: "unknown_permission"},
	})
}

func TestAdmin_Roles(t *testing.T) {
	runAdminCases(t, []adminCase{
		{name: "create", method: "POST", path: "/admin/v1/roles",
			body: `{"name":"auditor","description":"Audits"}`, status: http.StatusAccepted,
			event: "role.created", key: "registry", fields: map[string]any{"role_name": "auditor"}},
		{name: "create missing name", method: "POST", path: "/admin/v1/roles",
			body: `{}`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "create existing", method: "POST", path: "/admin/v1/roles",
			body: `{"name":"editor"}`, status: http.StatusConflict, code: "already_exists"},
	})
}

func TestAdmin_RolePermissions(t *testing.T) {
	runAdminCases(t, []adminCase{
		{name: "assign", method: "POST", path: "/admin/v1/roles/editor/permissions",
			body: `{"action":"read","resource_type":"document"}`, status: http.StatusAccepted,
			event: "role_permissions.assigned", key: "registry",
			fields: map[string]any{"role_name": "editor", "action": "read", "resource_type": "document"}},
		{name: "assign missing action", method: "POST", path: "/admin/v1/roles/editor/permissions",
			body: `{"resource_type":"document"}`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "assign missing resource_type", method: "POST", path: "/admin/v1/roles/editor/permissions",
			body: `{"action":"read"}`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "assign empty fields checked before the role lookup", method: "POST", path: "/admin/v1/roles/ghost/permissions",
			body: `{}`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "assign unknown role", method: "POST", path: "/admin/v1/roles/ghost/permissions",
			body: `{"action":"read","resource_type":"document"}`, status: http.StatusNotFound, code: "unknown_role"},
		{name: "assign unknown permission", method: "POST", path: "/admin/v1/roles/editor/permissions",
			body: `{"action":"fly","resource_type":"document"}`, status: http.StatusNotFound, code: "unknown_permission"},
		{name: "remove", method: "DELETE", path: "/admin/v1/roles/editor/permissions/read/document",
			status: http.StatusAccepted, event: "role_permissions.removed", key: "registry"},
		{name: "remove unknown role", method: "DELETE", path: "/admin/v1/roles/ghost/permissions/read/document",
			status: http.StatusNotFound, code: "unknown_role"},
		{name: "remove unknown permission", method: "DELETE", path: "/admin/v1/roles/editor/permissions/fly/document",
			status: http.StatusNotFound, code: "unknown_permission"},
	})
}

func TestAdmin_UserRoles(t *testing.T) {
	base := "/admin/v1/users/" + userID + "/roles"
	runAdminCases(t, []adminCase{
		{name: "assign", method: "POST", path: base,
			body:   `{"role_name":"editor","resource_type":"document","resource_id":"d1","scope":{"team":"a"},"granted_by":"` + actorID + `"}`,
			status: http.StatusAccepted, event: "role.assigned", key: userID,
			fields: map[string]any{"user_id": userID, "role_name": "editor", "resource_type": "document",
				"resource_id": "d1", "granted_by": actorID, "expires_at": nil}},
		{name: "assign with future expiry", method: "POST", path: base,
			body:   `{"role_name":"editor","resource_type":"document","resource_id":"d1","granted_by":"` + actorID + `","expires_at":"` + future + `"}`,
			status: http.StatusAccepted, event: "role.assigned", key: userID,
			fields: map[string]any{"expires_at": future}},
		{name: "assign canonicalises granted_by", method: "POST", path: base,
			body:   `{"role_name":"editor","resource_type":"document","resource_id":"d1","granted_by":"{00000000-0000-0000-0000-0000000000BB}"}`,
			status: http.StatusAccepted, event: "role.assigned", key: userID,
			fields: map[string]any{"granted_by": actorID}},
		{name: "assign invalid user_id", method: "POST", path: "/admin/v1/users/not-a-uuid/roles",
			body:   `{"role_name":"editor","resource_type":"document","resource_id":"d1","granted_by":"` + actorID + `"}`,
			status: http.StatusBadRequest, code: "invalid_request"},
		{name: "assign missing resource_type", method: "POST", path: base,
			body:   `{"role_name":"editor","resource_id":"d1","granted_by":"` + actorID + `"}`,
			status: http.StatusBadRequest, code: "invalid_request"},
		{name: "assign missing resource_id", method: "POST", path: base,
			body:   `{"role_name":"editor","resource_type":"document","granted_by":"` + actorID + `"}`,
			status: http.StatusBadRequest, code: "invalid_request"},
		{name: "assign missing granted_by", method: "POST", path: base,
			body:   `{"role_name":"editor","resource_type":"document","resource_id":"d1"}`,
			status: http.StatusBadRequest, code: "invalid_request"},
		{name: "assign invalid granted_by", method: "POST", path: base,
			body:   `{"role_name":"editor","resource_type":"document","resource_id":"d1","granted_by":"admin"}`,
			status: http.StatusBadRequest, code: "invalid_request"},
		{name: "assign past expiry", method: "POST", path: base,
			body:   `{"role_name":"editor","resource_type":"document","resource_id":"d1","granted_by":"` + actorID + `","expires_at":"` + past + `"}`,
			status: http.StatusBadRequest, code: "invalid_request"},
		{name: "assign malformed expiry", method: "POST", path: base,
			body:   `{"role_name":"editor","resource_type":"document","resource_id":"d1","granted_by":"` + actorID + `","expires_at":"tomorrow"}`,
			status: http.StatusBadRequest, code: "invalid_request"},
		{name: "assign unknown role", method: "POST", path: base,
			body:   `{"role_name":"ghost","resource_type":"document","resource_id":"d1","granted_by":"` + actorID + `"}`,
			status: http.StatusNotFound, code: "unknown_role"},
		{name: "revoke", method: "DELETE", path: base + "/editor/document/d1",
			status: http.StatusAccepted, event: "role.revoked", key: userID,
			fields: map[string]any{"user_id": userID, "role_name": "editor", "resource_type": "document", "resource_id": "d1"}},
		{name: "revoke unknown role", method: "DELETE", path: base + "/ghost/document/d1",
			status: http.StatusNotFound, code: "unknown_role"},
		{name: "revoke invalid user_id", method: "DELETE", path: "/admin/v1/users/nope/roles/editor/document/d1",
			status: http.StatusBadRequest, code: "invalid_request"},
	})
}

func TestAdmin_Overrides(t *testing.T) {
	base := "/admin/v1/users/" + userID + "/overrides"
	valid := func(extra string) string {
		return `{"action":"read","resource_type":"document","resource_id":"d1","effect":"DENY","reason":"r","set_by":"` + actorID + `"` + extra + `}`
	}
	runAdminCases(t, []adminCase{
		{name: "set deny", method: "POST", path: base, body: valid(""),
			status: http.StatusAccepted, event: "override.set", key: userID,
			fields: map[string]any{"user_id": userID, "action": "read", "resource_type": "document",
				"resource_id": "d1", "effect": "DENY", "set_by": actorID, "expires_at": nil}},
		{name: "set allow with future expiry", method: "POST", path: base,
			body:   `{"action":"read","resource_type":"document","resource_id":"d1","effect":"ALLOW","set_by":"` + actorID + `","expires_at":"` + future + `"}`,
			status: http.StatusAccepted, event: "override.set", key: userID,
			fields: map[string]any{"effect": "ALLOW", "expires_at": future}},
		{name: "set missing resource_id", method: "POST", path: base,
			body:   `{"action":"read","resource_type":"document","effect":"DENY","set_by":"` + actorID + `"}`,
			status: http.StatusBadRequest, code: "invalid_request"},
		{name: "set lowercase effect", method: "POST", path: base,
			body:   `{"action":"read","resource_type":"document","resource_id":"d1","effect":"deny","set_by":"` + actorID + `"}`,
			status: http.StatusBadRequest, code: "invalid_request"},
		{name: "set missing effect", method: "POST", path: base,
			body:   `{"action":"read","resource_type":"document","resource_id":"d1","set_by":"` + actorID + `"}`,
			status: http.StatusBadRequest, code: "invalid_request"},
		{name: "set unknown effect", method: "POST", path: base,
			body:   `{"action":"read","resource_type":"document","resource_id":"d1","effect":"MAYBE","set_by":"` + actorID + `"}`,
			status: http.StatusBadRequest, code: "invalid_request"},
		{name: "set missing set_by", method: "POST", path: base,
			body:   `{"action":"read","resource_type":"document","resource_id":"d1","effect":"DENY"}`,
			status: http.StatusBadRequest, code: "invalid_request"},
		{name: "set invalid set_by", method: "POST", path: base,
			body:   `{"action":"read","resource_type":"document","resource_id":"d1","effect":"DENY","set_by":"root"}`,
			status: http.StatusBadRequest, code: "invalid_request"},
		{name: "set past expiry", method: "POST", path: base, body: valid(`,"expires_at":"` + past + `"`),
			status: http.StatusBadRequest, code: "invalid_request"},
		{name: "set unknown permission", method: "POST", path: base,
			body:   `{"action":"fly","resource_type":"document","resource_id":"d1","effect":"DENY","set_by":"` + actorID + `"}`,
			status: http.StatusNotFound, code: "unknown_permission"},
		{name: "set invalid user_id", method: "POST", path: "/admin/v1/users/x/overrides", body: valid(""),
			status: http.StatusBadRequest, code: "invalid_request"},
		{name: "remove", method: "DELETE", path: base + "/read/document/d1",
			status: http.StatusAccepted, event: "override.removed", key: userID,
			fields: map[string]any{"user_id": userID, "action": "read", "resource_type": "document", "resource_id": "d1"}},
		{name: "remove unknown permission", method: "DELETE", path: base + "/fly/document/d1",
			status: http.StatusNotFound, code: "unknown_permission"},
		{name: "remove invalid user_id", method: "DELETE", path: "/admin/v1/users/x/overrides/read/document/d1",
			status: http.StatusBadRequest, code: "invalid_request"},
	})
}

func TestAdmin_OversizeBodyRejected(t *testing.T) {
	paths := []struct {
		name, path, field string
	}{
		{"create action", "/admin/v1/actions", "description"},
		{"create role", "/admin/v1/roles", "description"},
		{"assign role permission", "/admin/v1/roles/editor/permissions", "action"},
		{"assign user role", "/admin/v1/users/" + userID + "/roles", "resource_id"},
		{"set override", "/admin/v1/users/" + userID + "/overrides", "reason"},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			ts := newTestServer(t)
			rec := ts.do(t, "POST", p.path, oversizeJSON(p.field))
			e := requireError(t, rec, http.StatusBadRequest, "invalid_request")
			if e.Message != "request body too large" {
				t.Errorf("message = %q, want %q", e.Message, "request body too large")
			}
			if len(ts.pub.events) != 0 {
				t.Fatal("oversize request published an event")
			}
		})
	}
}

func TestAdmin_BodyAtLimitAccepted(t *testing.T) {
	ts := newTestServer(t)
	// Exactly maxBodyBytes in total.
	prefix := `{"action":"write","resource_type":"document","description":"`
	suffix := `"}`
	pad := make([]byte, maxBodyBytes-len(prefix)-len(suffix))
	for i := range pad {
		pad[i] = 'a'
	}
	rec := ts.do(t, "POST", "/admin/v1/actions", prefix+string(pad)+suffix)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 for a body of exactly 1 MiB (body %s)", rec.Code, rec.Body.String())
	}
}

func TestAdmin_PublishFailure(t *testing.T) {
	ts := newTestServer(t)
	ts.pub.err = errBoom
	rec := ts.do(t, "POST", "/admin/v1/roles", `{"name":"auditor"}`)
	requireError(t, rec, http.StatusInternalServerError, "internal_error")
}

func TestAdmin_Reads(t *testing.T) {
	t.Run("list actions", func(t *testing.T) {
		ts := newTestServer(t)
		ts.store.perms = []model.Permission{{Action: "read", ResourceType: "document"}}
		rec := ts.do(t, "GET", "/admin/v1/actions", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		got := decodeBody[[]model.Permission](t, rec)
		if len(got) != 1 || got[0].Action != "read" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("list actions empty is an array", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, "GET", "/admin/v1/actions", "")
		if rec.Code != http.StatusOK || rec.Body.String() != "[]\n" {
			t.Fatalf("status = %d body = %q, want 200 []", rec.Code, rec.Body.String())
		}
	})

	t.Run("list roles", func(t *testing.T) {
		ts := newTestServer(t)
		ts.store.roles = []model.Role{{Name: "editor"}}
		rec := ts.do(t, "GET", "/admin/v1/roles", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := decodeBody[[]model.Role](t, rec); len(got) != 1 || got[0].Name != "editor" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("list role permissions resolves the role id", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, "GET", "/admin/v1/roles/editor/permissions", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if len(ts.store.roleIDs) != 1 || ts.store.roleIDs[0] != 1 {
			t.Fatalf("ListRolePermissions role ids = %v, want [1]", ts.store.roleIDs)
		}
	})

	t.Run("list role permissions unknown role", func(t *testing.T) {
		ts := newTestServer(t)
		requireError(t, ts.do(t, "GET", "/admin/v1/roles/ghost/permissions", ""), http.StatusNotFound, "unknown_role")
	})

	t.Run("store error", func(t *testing.T) {
		ts := newTestServer(t)
		ts.store.err = errBoom
		for _, p := range []string{"/admin/v1/actions", "/admin/v1/roles", "/admin/v1/roles/editor/permissions"} {
			requireError(t, ts.do(t, "GET", p, ""), http.StatusInternalServerError, "internal_error")
		}
	})
}

func TestAdmin_Auth(t *testing.T) {
	tests := []struct {
		name   string
		token  string
		status int
		code   string
	}{
		{"missing token", "", http.StatusUnauthorized, "missing_token"},
		{"wrong token", "s3cret-tokeX", http.StatusForbidden, "invalid_token"},
		{"shorter token", "s3cret", http.StatusForbidden, "invalid_token"},
		{"longer token", adminToken + "x", http.StatusForbidden, "invalid_token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t)
			rec := ts.do(t, "POST", "/admin/v1/roles", `{"name":"auditor"}`, "X-Admin-Token", tt.token)
			requireError(t, rec, tt.status, tt.code)
			if len(ts.pub.events) != 0 {
				t.Fatal("unauthenticated request published an event")
			}
		})
	}

	t.Run("valid token passes", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, "POST", "/admin/v1/roles", `{"name":"auditor"}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rec.Code)
		}
	})

	t.Run("admin routes absent when admin mode is off", func(t *testing.T) {
		rec := newQueryOnlyServer(t).do(t, "GET", "/admin/v1/actions", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 with admin mode off", rec.Code)
		}
	})
}
