package api

import (
	"net/http"

	"authz-service/internal/config"
	"authz-service/internal/resolver"

	"github.com/go-chi/chi/v5"
	httpSwagger "github.com/swaggo/http-swagger"
)

func NewRouter(res *resolver.Resolver, adminHandler *AdminHandler, cfg *config.Config) http.Handler {
	r := chi.NewRouter()
	r.Use(RequestIDMiddleware, LoggingMiddleware, RecoveryMiddleware)

	r.Mount("/query/v1", queryRouter(res))

	if cfg.AdminMode {
		r.Mount("/admin/v1", adminRouter(adminHandler, cfg.AdminToken))
	}

	r.Get("/healthz", handleLiveness)
	r.Get("/readyz", handleReadiness)

	r.Get("/swagger/*", httpSwagger.WrapHandler)

	return r
}

func queryRouter(res *resolver.Resolver) http.Handler {
	r := chi.NewRouter()
	r.Get("/check", handleCheck(res))
	r.Get("/permissions", handlePermissions(res))
	r.Get("/resources", handleResources(res))
	r.Post("/resources/filter", handleFilter(res))
	return r
}

func adminRouter(h *AdminHandler, token string) http.Handler {
	r := chi.NewRouter()
	r.Use(adminAuthMiddleware(token))

	// Actions
	r.Get("/actions", h.ListActions)
	r.Post("/actions", h.CreateAction)
	r.Delete("/actions/{action}/{resource_type}", h.DeleteAction)

	// Roles
	r.Get("/roles", h.ListRoles)
	r.Post("/roles", h.CreateRole)
	r.Get("/roles/{name}/permissions", h.ListRolePermissions)
	r.Post("/roles/{name}/permissions", h.AssignRolePermission)
	r.Delete("/roles/{name}/permissions/{action}/{resource_type}", h.RemoveRolePermission)

	// User roles
	r.Post("/users/{user_id}/roles", h.AssignUserRole)
	r.Delete("/users/{user_id}/roles/{role_name}/{resource_type}/{resource_id}", h.RevokeUserRole)

	// User overrides
	r.Post("/users/{user_id}/overrides", h.SetOverride)
	r.Delete("/users/{user_id}/overrides/{action}/{resource_type}/{resource_id}", h.RemoveOverride)

	return r
}

func adminAuthMiddleware(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t := r.Header.Get("X-Admin-Token")
			if t == "" {
				writeError(w, http.StatusUnauthorized, "missing_token", "X-Admin-Token header is required")
				return
			}
			if t != token {
				writeError(w, http.StatusForbidden, "invalid_token", "invalid admin token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
