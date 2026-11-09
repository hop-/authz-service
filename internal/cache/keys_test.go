package cache

import (
	"testing"

	"authz-service/internal/model"

	"github.com/google/uuid"
)

var keyUser = uuid.MustParse("00000000-0000-0000-0000-0000000000aa")

func req(resourceType, resourceID, action string) model.CheckRequest {
	return model.CheckRequest{UserID: keyUser, ResourceType: resourceType, ResourceID: resourceID, Action: action}
}

// AC-8: each key component escapes \ and :, so ":" only separates components.
func TestCheckKey_Escaping(t *testing.T) {
	c := &Cache{}
	tests := []struct {
		name string
		req  model.CheckRequest
		want string
	}{
		{"plain", req("document", "d1", "read"),
			`authz:check:00000000-0000-0000-0000-0000000000aa:document:d1:read`},
		{"colon in id", req("document", "doc:1", "read"),
			`authz:check:00000000-0000-0000-0000-0000000000aa:document:doc\:1:read`},
		{"colon in type and action", req("boards:pin", "x", "boards:read"),
			`authz:check:00000000-0000-0000-0000-0000000000aa:boards\:pin:x:boards\:read`},
		{"backslash", req("document", `a\b`, "read"),
			`authz:check:00000000-0000-0000-0000-0000000000aa:document:a\\b:read`},
		{"backslash before colon", req("document", `a\:b`, "read"),
			`authz:check:00000000-0000-0000-0000-0000000000aa:document:a\\\:b:read`},
		{"glob characters are literal in keys", req("document", "d*?[1]", "read"),
			`authz:check:00000000-0000-0000-0000-0000000000aa:document:d*?[1]:read`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.checkKey(tt.req); got != tt.want {
				t.Fatalf("checkKey = %q, want %q", got, tt.want)
			}
		})
	}
}

// AC-8: requests that used to share a key ("a:b" + "c" vs "a" + "b:c") don't.
func TestCheckKey_NoCollisions(t *testing.T) {
	c := &Cache{}
	pairs := [][2]model.CheckRequest{
		{req("a:b", "c", "read"), req("a", "b:c", "read")},
		{req("document", "x:read", "y"), req("document", "x", "read:y")},
		{req("document", `x\`, "read"), req("document", `x\\`, "read")},
		{req("document", `x\:y`, "read"), req("document", `x:y`, "read")},
	}
	for _, p := range pairs {
		if a, b := c.checkKey(p[0]), c.checkKey(p[1]); a == b {
			t.Errorf("%+v and %+v share the key %q", p[0], p[1], a)
		}
	}
}

// AC-8: the SCAN pattern escapes glob metacharacters on top of the key
// escaping, and only the action is a wildcard.
func TestInvalidatePattern(t *testing.T) {
	u := keyUser.String()
	tests := []struct {
		name             string
		resourceType, id string
		want             string
	}{
		{"plain", "document", "d1", `authz:check:` + u + `:document:d1:*`},
		{"colon", "document", "doc:1", `authz:check:` + u + `:document:doc\\:1:*`},
		{"star", "document", "doc:*", `authz:check:` + u + `:document:doc\\:\*:*`},
		{"all glob metacharacters", "t?", "[a]*", `authz:check:` + u + `:t\?:\[a\]\*:*`},
		{"backslash", "document", `a\b`, `authz:check:` + u + `:document:a\\\\b:*`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := invalidatePattern(u, tt.resourceType, tt.id); got != tt.want {
				t.Fatalf("pattern = %q, want %q", got, tt.want)
			}
		})
	}
}
