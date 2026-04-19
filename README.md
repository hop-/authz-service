# authz-service

Standalone authorization microservice. Stores and evaluates user permissions for arbitrary resources and actions using RBAC with resource-scoped roles and direct user overrides.

## Architecture

| Component | Role |
| --- | --- |
| **authz-api** | HTTP server. Query endpoints (always) + admin endpoints (when `ADMIN_MODE=true`). Reads from DB replica only. |
| **authz-worker** | Kafka consumer. Sole writer to PostgreSQL. Processes all permission mutations, invalidates Redis cache. |

## Quick Start

```bash
# setup .env files
cp .env.dist .env
# (edit .env)
cp .env.dist .env.worker
# (edit .env.worker)
cp .env.dist .env.api
# (edit .env.api)

# Start dependencies
docker compose up -d
```

TODO: add migration instructions

## API

### Query (always active)

```none
GET  /query/v1/check?user_id=<uuid>&action=read&resource_type=document&resource_id=doc:123
GET  /query/v1/permissions?user_id=<uuid>
GET  /query/v1/resources?user_id=<uuid>&action=read&resource_type=document
POST /query/v1/resources/filter
```

### Admin (`ADMIN_MODE=true`, requires `X-Admin-Token` header)

```none
GET|POST|DELETE  /admin/v1/actions
GET|POST         /admin/v1/roles
GET|POST|DELETE  /admin/v1/roles/{name}/permissions
POST|DELETE      /admin/v1/users/{user_id}/roles
POST|DELETE      /admin/v1/users/{user_id}/overrides
```

### Health

```none
GET /healthz   — liveness (always 200)
GET /readyz    — readiness (checks DB, Redis, registry)
```

### Swagger / OpenAPI

Auto-generated docs are served at `/swagger/index.html` when the API is running.

To regenerate after code changes:

```bash
swag init -g cmd/api/main.go -o docs/ --parseDependency --parseInternal
```

The generated spec files live in `docs/` (`swagger.json`, `swagger.yaml`). These are committed so the API image serves them without needing the `swag` CLI at runtime.

## Configuration

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `DATABASE_URL` | yes | — | PostgreSQL connection string |
| `REDIS_ADDR` | yes | — | Redis address |
| `KAFKA_BROKERS` | yes | — | Comma-separated broker list |
| `KAFKA_TOPIC` | no | `authz.permission-events` | Kafka topic |
| `HTTP_PORT` | no | `3000` | API listen port |
| `METRICS_PORT` | no | `3001` | Prometheus metrics port |
| `ADMIN_MODE` | no | `false` | Mount admin endpoints |
| `ADMIN_TOKEN` | when admin | — | Token for admin auth |
| `CACHE_TTL_SECONDS` | no | `300` | Redis cache TTL |
| `DB_MAX_CONNS` | no | `20` | Max DB pool connections |

## Permission Model

Resolution order (on cache miss):

1. **DENY override** → denied
2. **ALLOW override** → allowed
3. **Role check** → allowed
4. **Default** → denied

## Build

```bash
go build ./cmd/api
go build ./cmd/worker
go test ./...
```

## Testing

```bash
# Unit tests
go test ./...
```

## Docker

```bash
# API image
docker build --target api -t authz-api .

# Worker image
docker build --target worker -t authz-worker .
```
