//go:build integration

package resolver_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"authz-service/internal/db"
	"authz-service/internal/model"

	"github.com/google/uuid"
)

func (e env) grantOn(t *testing.T, user uuid.UUID, resourceType, resourceID string, expiresAt *time.Time) {
	t.Helper()
	ctx := context.Background()
	rid, found, err := e.q.RoleID(ctx, "viewer")
	if err != nil || !found {
		t.Fatalf("RoleID = (%d, %v, %v)", rid, found, err)
	}
	if applied, err := e.q.ApplyEvent(ctx, uuid.New(), "role.assigned", func(w db.Writer) error {
		return w.InsertUserRole(ctx, user, rid, resourceType, resourceID, nil, actor, time.Now(), expiresAt)
	}); err != nil || !applied {
		t.Fatalf("grant %s: (%v, %v)", resourceID, applied, err)
	}
}

func (e env) overrideOn(t *testing.T, user uuid.UUID, resourceID string, effect model.Effect, expiresAt *time.Time) {
	t.Helper()
	ctx := context.Background()
	pid, found, err := e.q.PermissionID(ctx, "read", "document")
	if err != nil || !found {
		t.Fatalf("PermissionID = (%d, %v, %v)", pid, found, err)
	}
	if applied, err := e.q.ApplyEvent(ctx, uuid.New(), "override.set", func(w db.Writer) error {
		return w.UpsertOverride(ctx, user, pid, resourceID, nil, effect, "test", actor, time.Now(), expiresAt)
	}); err != nil || !applied {
		t.Fatalf("override %s: (%v, %v)", resourceID, applied, err)
	}
}

// AC-6: Filter (now SQL-side) gives the same answer as the previous
// semantics, Resources ∩ candidates, including DENY overrides, and keeps
// the candidates' order. Every candidate is also checked against Check.
func TestFilter_EquivalentToResourcesAndCheck(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user := uuid.New()
	for _, id := range []string{"a", "b", "c", "d:1", "e*"} {
		e.grantOn(t, user, "document", id, nil)
	}
	e.grantOn(t, user, "document", "expired", in(-time.Minute))
	e.grantOn(t, user, "project", "p", nil)
	e.overrideOn(t, user, "b", model.EffectDeny, nil)              // DENY beats the role
	e.overrideOn(t, user, "z", model.EffectAllow, nil)             // ALLOW without a role
	e.overrideOn(t, user, "c", model.EffectDeny, in(-time.Minute)) // expired DENY: c stays allowed

	candidates := []string{"z", "e*", "b", "missing", "expired", "p", "d:1", "a", "c", "a"}

	got, err := e.res.Filter(ctx, user, "read", "document", candidates)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}

	all, err := e.res.Resources(ctx, model.ResourcesRequest{UserID: user, Action: "read", ResourceType: "document"})
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	var want []string
	for _, c := range candidates {
		if slices.Contains(all.ResourceIDs, c) {
			want = append(want, c)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Filter = %v, want %v (previous semantics, candidate order)", got, want)
	}
	if !slices.Equal(want, []string{"z", "e*", "d:1", "a", "c", "a"}) {
		t.Fatalf("fixture sanity: expected allowed set = %v", want)
	}

	for _, c := range slices.Compact(slices.Clone(candidates)) {
		res, err := e.res.Check(ctx, model.CheckRequest{UserID: user, Action: "read", ResourceType: "document", ResourceID: c})
		if err != nil {
			t.Fatalf("Check %s: %v", c, err)
		}
		if res.Allowed != slices.Contains(got, c) {
			t.Errorf("Check(%s).Allowed = %v but Filter says %v", c, res.Allowed, slices.Contains(got, c))
		}
	}
}
