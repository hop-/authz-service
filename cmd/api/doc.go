// Package main is the entrypoint for authz-api.
//
//	@title			Authz Service API
//	@version		1.0
//	@description	Authorization microservice with RBAC, resource-scoped roles, and user permission overrides.
//
//	@host		localhost:8080
//	@BasePath	/
//
//	@securityDefinitions.apikey	AdminToken
//	@in							header
//	@name						X-Admin-Token
//	@description				Admin bearer token for mutation endpoints
package main
