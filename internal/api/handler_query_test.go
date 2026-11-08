package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"authz-service/internal/model"
)

func TestCheckHandler(t *testing.T) {
	q := func(v url.Values) string { return "/query/v1/check?" + v.Encode() }
	valid := url.Values{"user_id": {userID}, "action": {"read"}, "resource_type": {"document"}, "resource_id": {"d1"}}
	without := func(key string) url.Values {
		v := url.Values{}
		for k, vs := range valid {
			if k != key {
				v[k] = vs
			}
		}
		return v
	}

	for _, key := range []string{"user_id", "action", "resource_type", "resource_id"} {
		t.Run("missing "+key, func(t *testing.T) {
			ts := newTestServer(t)
			requireError(t, ts.do(t, "GET", q(without(key)), ""), http.StatusBadRequest, "invalid_request")
		})
	}

	t.Run("invalid user_id", func(t *testing.T) {
		ts := newTestServer(t)
		v := without("user_id")
		v.Set("user_id", "nope")
		requireError(t, ts.do(t, "GET", q(v), ""), http.StatusBadRequest, "invalid_request")
	})

	t.Run("unknown permission", func(t *testing.T) {
		ts := newTestServer(t)
		v := without("action")
		v.Set("action", "fly")
		requireError(t, ts.do(t, "GET", q(v), ""), http.StatusNotFound, "unknown_permission")
	})

	t.Run("allowed by role", func(t *testing.T) {
		ts := newTestServer(t)
		ts.rstore.roleName = "editor"
		rec := ts.do(t, "GET", q(valid), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
		}
		got := decodeBody[model.CheckResult](t, rec)
		if got != (model.CheckResult{Allowed: true, Source: "role:editor"}) {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("store error", func(t *testing.T) {
		ts := newTestServer(t)
		ts.rstore.err = errBoom
		requireError(t, ts.do(t, "GET", q(valid), ""), http.StatusInternalServerError, "internal_error")
	})
}

func TestPermissionsHandler(t *testing.T) {
	t.Run("missing user_id", func(t *testing.T) {
		ts := newTestServer(t)
		requireError(t, ts.do(t, "GET", "/query/v1/permissions", ""), http.StatusBadRequest, "invalid_request")
	})

	t.Run("invalid user_id", func(t *testing.T) {
		ts := newTestServer(t)
		requireError(t, ts.do(t, "GET", "/query/v1/permissions?user_id=x", ""), http.StatusBadRequest, "invalid_request")
	})

	t.Run("empty is an array", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, "GET", "/query/v1/permissions?user_id="+userID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"permissions":[]}` {
			t.Fatalf("body = %s, want {\"permissions\":[]}", got)
		}
	})

	t.Run("effective entries", func(t *testing.T) {
		ts := newTestServer(t)
		ts.rstore.grants = []model.RoleGrant{
			{Action: "read", ResourceType: "document", ResourceID: "d1", RoleName: "editor"},
			{Action: "read", ResourceType: "document", ResourceID: "d2", RoleName: "editor"},
		}
		ts.rstore.overrides = []model.OverrideGrant{
			{Action: "read", ResourceType: "document", ResourceID: "d2", Effect: model.EffectDeny},
			{Action: "write", ResourceType: "document", ResourceID: "d1", Effect: model.EffectAllow},
		}
		rec := ts.do(t, "GET", "/query/v1/permissions?user_id="+userID+"&resource_type=document", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		got := decodeBody[map[string][]model.PermissionEntry](t, rec)["permissions"]
		want := []string{"read/d1/role:editor", "write/d1/allow_override"}
		if len(got) != len(want) {
			t.Fatalf("got %+v, want %v", got, want)
		}
		for i, e := range got {
			if s := fmt.Sprintf("%s/%s/%s", e.Action, e.ResourceID, e.Source); s != want[i] {
				t.Errorf("entry %d = %s, want %s", i, s, want[i])
			}
		}
	})

	t.Run("store error", func(t *testing.T) {
		ts := newTestServer(t)
		ts.rstore.err = errBoom
		requireError(t, ts.do(t, "GET", "/query/v1/permissions?user_id="+userID, ""), http.StatusInternalServerError, "internal_error")
	})
}

func TestResourcesHandler(t *testing.T) {
	base := "/query/v1/resources?user_id=" + userID + "&action=read&resource_type=document"

	tests := []struct {
		name   string
		path   string
		status int
		code   string
	}{
		{"missing user_id", "/query/v1/resources?action=read&resource_type=document", http.StatusBadRequest, "invalid_request"},
		{"missing action", "/query/v1/resources?user_id=" + userID + "&resource_type=document", http.StatusBadRequest, "invalid_request"},
		{"missing resource_type", "/query/v1/resources?user_id=" + userID + "&action=read", http.StatusBadRequest, "invalid_request"},
		{"invalid user_id", "/query/v1/resources?user_id=x&action=read&resource_type=document", http.StatusBadRequest, "invalid_request"},
		{"bad scope_filter", base + "&scope_filter=" + url.QueryEscape(`{"team":1}`), http.StatusBadRequest, "invalid_request"},
		{"unknown permission", "/query/v1/resources?user_id=" + userID + "&action=fly&resource_type=document", http.StatusNotFound, "unknown_permission"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t)
			requireError(t, ts.do(t, "GET", tt.path, ""), tt.status, tt.code)
		})
	}

	t.Run("ok with scope filter", func(t *testing.T) {
		ts := newTestServer(t)
		ts.rstore.resourceIDs = []string{"d1"}
		rec := ts.do(t, "GET", base+"&scope_filter="+url.QueryEscape(`{"team":"a"}`), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
		}
		if got := decodeBody[model.ResourcesResult](t, rec); len(got.ResourceIDs) != 1 || got.ResourceIDs[0] != "d1" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("store error", func(t *testing.T) {
		ts := newTestServer(t)
		ts.rstore.err = errBoom
		requireError(t, ts.do(t, "GET", base, ""), http.StatusInternalServerError, "internal_error")
	})
}

func TestFilterHandler(t *testing.T) {
	const path = "/query/v1/resources/filter"
	ids := make([]string, 1001)
	for i := range ids {
		ids[i] = fmt.Sprintf(`"r%d"`, i)
	}
	tooMany := `{"user_id":"` + userID + `","action":"read","resource_type":"document","candidate_ids":[` + strings.Join(ids, ",") + `]}`

	tests := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"invalid JSON", `{"user_id":`, http.StatusBadRequest, "invalid_request"},
		{"missing user_id", `{"action":"read","resource_type":"document"}`, http.StatusBadRequest, "invalid_request"},
		{"missing action", `{"user_id":"` + userID + `","resource_type":"document"}`, http.StatusBadRequest, "invalid_request"},
		{"invalid user_id", `{"user_id":"x","action":"read","resource_type":"document"}`, http.StatusBadRequest, "invalid_request"},
		{"too many candidates", tooMany, http.StatusBadRequest, "invalid_request"},
		{"unknown permission", `{"user_id":"` + userID + `","action":"fly","resource_type":"document","candidate_ids":["d1"]}`, http.StatusNotFound, "unknown_permission"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t)
			requireError(t, ts.do(t, "POST", path, tt.body), tt.status, tt.code)
		})
	}

	t.Run("oversize body", func(t *testing.T) {
		ts := newTestServer(t)
		e := requireError(t, ts.do(t, "POST", path, oversizeJSON("user_id")), http.StatusBadRequest, "invalid_request")
		if e.Message != "request body too large" {
			t.Errorf("message = %q, want %q", e.Message, "request body too large")
		}
	})

	t.Run("intersection", func(t *testing.T) {
		ts := newTestServer(t)
		ts.rstore.resourceIDs = []string{"d2", "d3"}
		rec := ts.do(t, "POST", path, `{"user_id":"`+userID+`","action":"read","resource_type":"document","candidate_ids":["d1","d2","d3"]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
		}
		got := decodeBody[map[string][]string](t, rec)["allowed_ids"]
		if strings.Join(got, ",") != "d2,d3" {
			t.Fatalf("allowed_ids = %v, want [d2 d3]", got)
		}
	})

	t.Run("store error", func(t *testing.T) {
		ts := newTestServer(t)
		ts.rstore.err = errBoom
		rec := ts.do(t, "POST", path, `{"user_id":"`+userID+`","action":"read","resource_type":"document","candidate_ids":["d1"]}`)
		requireError(t, rec, http.StatusInternalServerError, "internal_error")
	})
}
