# authz-service — LLM code-generation specification

> **How to use this document.** This is a structured prompt for one or more LLM agents to generate production-quality Go code for the `authz-service`. Each numbered section maps to one or more output files. Sections are self-contained: an agent assigned a section only needs the sections listed in its **Depends on** line plus the global rules in §2.
>
> **Conventions used throughout:**
>
> - `GENERATE` blocks contain Go code or SQL that must appear verbatim in the output (signatures, struct definitions, SQL DDL). Free-form prose around them is context — do not emit it as comments unless explicitly instructed.
> - `> Design note:` blockquotes are background rationale. They explain *why* a decision was made. Do not generate code from them — they exist to prevent an agent from "improving" the design in ways that break assumptions elsewhere.
> - All JSON-serialized Go struct fields use `snake_case` tags matching the JSON examples in this document. Add `json:"field_name"` tags to every exported struct field. Omit tags on unexported fields.
> - All error responses use a standard envelope: `{ "error": { "code": "short_code", "message": "Human-readable detail" } }`. Error codes are `snake_case`. HTTP handlers must never return a bare string or an unstructured JSON body on error.

---

## 1. Project overview

**Service name:** `authz-service`

**Purpose:** A standalone microservice that stores and evaluates user permissions for arbitrary resources and actions. It is the single source of truth for authorization decisions across the platform. The service is **feature-agnostic** — it does not know the semantics of any action. Actions are opaque strings registered by consuming application teams when new features ship.

**Architecture:** Two independently deployable components sharing one database:

| Component | Role | DB access | Kafka role |
| --- | --- | --- | --- |
| `authz-api` | HTTP server. `/query/v1/...` (always) + `/admin/v1/...` (when `ADMIN_MODE=true`). | Read replica only | Consumer (registry updates) + Producer (admin mutations, when `ADMIN_MODE=true`) |
| `authz-worker` | Kafka consumer. Sole writer to PostgreSQL. | Primary (read-write) | Consumer (all events) + Producer (`*.id.assigned` internal events) |

**Language:** Go (latest stable)

**Key non-functional requirements:**

- Read/write ratio: ~100:1 — optimize aggressively for reads.
- Permission check p99 latency target: < 10 ms (cache hit), < 40 ms (cache miss).
- All writes are idempotent — Kafka at-least-once delivery is assumed; the worker must handle duplicate messages safely.
- All permission grants and overrides must be auditable (who granted, when, why).

---

## 2. Global code-generation rules

These rules apply to every section and every generated file. Read them before generating any code.

1. **One file per concern** — generate each internal package as a separate file. Do not collapse everything into `main.go`.
2. **Errors are explicit** — never `panic` in library code. Return `error` from all fallible functions.
3. **Context everywhere** — every function that touches DB, Redis, or network must accept `context.Context` as its first argument.
4. **No global state** — use dependency injection. The resolver receives cache and db as constructor arguments.
5. **No ORM** — write raw SQL in `internal/db/queries.go` using `pgx/v5` positional arguments (`$1`, `$2`, …). Do not use GORM, sqlboiler, or ent.
6. **Natural keys in external interfaces** — all APIs, Kafka events, and log fields use natural keys (`role_name`, `action`, `resource_type`). SMALLINT surrogate IDs are an internal DB implementation detail and must never appear in HTTP responses or Kafka event payloads, except in internal `action.id.assigned` and `role.id.assigned` events which carry the surrogate ID specifically for registry consumers.
7. **JSON tags on every exported struct field** — use `snake_case` tag names matching the JSON examples in this document. Example: `Action string \`json:"action"\``.
8. **Standard error response** — all HTTP error responses use `{ "error": { "code": "...", "message": "..." } }`. Define an `ErrorResponse` struct in `internal/model/model.go` and a `writeError(w, status, code, msg)` helper in `internal/api/`.
9. **Registry is initialized before anything else** — both `cmd/api/main.go` and `cmd/worker/main.go` must load the registry from DB before starting the HTTP server or consuming Kafka messages. If registry load fails, the process must exit.
10. **UUID v7 for generated IDs** — use `uuid.NewV7()` from `github.com/google/uuid` v1.6+ when authz-service generates a UUID. External IDs (`user_id`, `granted_by`, etc.) are accepted and stored as-is.
11. **Admin mode enforcement** — in `router.go`, only mount `/admin/v1` routes when `cfg.AdminMode == true`. Do not register them and hide them behind a 403 — they must be absent entirely. When `AdminMode=true`, validate at startup that `ADMIN_TOKEN` and `KAFKA_BROKERS` are non-empty — refuse to start if either is missing. Initialize the Kafka producer client at startup; if the producer fails to connect, refuse to start.
12. **Admin API is a pure event producer** — admin handler functions must never call any `db.Insert*`, `db.Update*`, or `db.Delete*` function. The only DB access from admin handlers is read-only (registry is in-memory; GET endpoints read from replica). All mutations are expressed as Kafka events published via the producer. The worker is the only code path that writes to PostgreSQL.
13. **DB pool via pgxpool** — configure `pgxpool.Config` from all `DB*` config fields. Apply `AfterConnect` hook for read-only session enforcement. Expose pool stats (`pgxpool.Pool.Stat()`) as Prometheus gauges: `authz_db_pool_total_conns`, `authz_db_pool_idle_conns`, `authz_db_pool_acquired_conns`.
14. **Tests** — generate `_test.go` files for the resolver that unit-test each of the 5 resolution steps using mock DB and mock cache interfaces.
15. **Graceful shutdown** — both `cmd/api/main.go` and `cmd/worker/main.go` must handle `SIGTERM` and `SIGINT`, draining in-flight requests and committing Kafka offsets before exit.
16. **Dockerfile** — multi-stage build: `golang:1.23-alpine` builder + `gcr.io/distroless/static` runtime. Produce two separate images, one per binary (`cmd/api`, `cmd/worker`).
17. **Structured logging** — use `log/slog` (Go 1.21+) with JSON output. Every log entry must include `service` (`authz-api` or `authz-worker`), `request_id` (per HTTP request), `user_id` (when available), `event_id` (in worker entries).

---

## 3. File → section dependency map

This table tells each agent which sections it needs to generate a given file. An agent can be assigned one or more rows. It only needs to read the sections in the **Input sections** column.

| Output file | Package | Input sections | Description |
| --- | --- | --- | --- |
| `internal/model/model.go` | model | §2, §4 | Domain types, sentinel errors, error response |
| `internal/config/config.go` | config | §2, §5 | Env-based config struct |
| `internal/db/postgres.go` | db | §2, §5, §6 | pgxpool setup, AfterConnect hook, replica check |
| `internal/db/queries.go` | db | §2, §4, §6 | All raw SQL queries |
| `internal/cache/redis.go` | cache | §2, §4, §7 | Redis get/set/invalidate/flush |
| `internal/registry/registry.go` | registry | §2, §4, §8 | In-memory permission+role maps |
| `internal/resolver/resolver.go` | resolver | §2, §4, §7, §8, §9 | 5-step check algorithm, list, resources, filter |
| `internal/resolver/resolver_test.go` | resolver | §2, §4, §9 | Unit tests for all 5 resolution steps |
| `internal/api/router.go` | api | §2, §5, §10 | Chi router, mount logic |
| `internal/api/middleware.go` | api | §2 | Request-ID, logging, recovery |
| `internal/api/errors.go` | api | §2, §4 | writeError helper, standard error codes |
| `internal/api/handler_check.go` | api | §2, §4, §9, §10 | GET /query/v1/check |
| `internal/api/handler_permissions.go` | api | §2, §4, §9, §10 | GET /query/v1/permissions |
| `internal/api/handler_resources.go` | api | §2, §4, §9, §10 | GET /query/v1/resources, POST filter |
| `internal/api/handler_admin.go` | api | §2, §4, §8, §10, §11 | All /admin/v1 endpoints |
| `internal/api/handler_health.go` | api | §2, §5, §10 | /healthz, /readyz |
| `internal/producer/producer.go` | producer | §2, §5, §11 | Kafka producer wrapper |
| `internal/worker/consumer.go` | worker | §2, §5, §12 | Kafka consumer loop, offset commit |
| `internal/worker/handler.go` | worker | §2, §4, §7, §8, §11, §12 | Event dispatch, DB writes, cache invalidation |
| `cmd/api/main.go` | main | §2, §5, §8, §10, §13 | API startup, shutdown |
| `cmd/worker/main.go` | main | §2, §5, §8, §12, §13 | Worker startup, shutdown |
| `migrations/001_initial.up.sql` | — | §6 | DDL |
| `migrations/002_seed.up.sql` | — | §6 | Bootstrap data |
| `Dockerfile` | — | §2 | Multi-stage build |
| `docker-compose.yml` | — | §5 | Local dev environment |

---

## 4. Permission model and domain types

**Depends on:** §2

### Core concepts

| Concept | Description |
| --- | --- |
| **Action** | An opaque string registered by the application team, e.g. `list`, `get`, `create`, `update`, `delete`, `pin`. The authz service assigns no meaning to action strings. |
| **Resource type** | A category of resource, e.g. `document`, `project`, `board`. |
| **Resource ID** | Instance identifier (e.g. UUID string). Combined with resource type to form a full resource reference. |
| **Permission** | A registered `(action, resource_type)` pair — the unit of assignment. |
| **Role** | A named, reusable collection of permissions. May span multiple resource types, but only the permissions matching the assigned resource type are evaluated (see §17). |
| **User role assignment** | Grants a user a role scoped to a specific resource instance. |
| **Permission override** | A direct per-user, per-permission, per-resource grant or denial. Bypasses role evaluation. Effect is `ALLOW` or `DENY`. |

### Action lifecycle

1. Register action: `POST /admin/v1/actions` → `202`. Worker writes to DB, publishes `action.id.assigned`.
2. Assign to roles: `POST /admin/v1/roles/{name}/permissions` → `202`. May fail with `404` if step 1 hasn't been processed. Caller retries with backoff (500 ms, up to 3 attempts).
3. Application queries: `GET /query/v1/check?action=pin&resource_type=board&...`
4. No authz-service code change required.

### Resolution algorithm

Evaluated in this exact order on every cache miss:

| Step | Logic | Result source label |
| --- | --- | --- |
| 0 | Resolve `(action, resource_type)` → `permission_id` via registry. If unknown → return `ErrUnknownPermission` | `"unknown_permission"` |
| 1 | Cache lookup. If hit → return cached result | `"cache"` |
| 2 | Query `user_permission_overrides` for `DENY`. If found → denied | `"deny_override"` |
| 3 | Query `user_permission_overrides` for `ALLOW`. If found → allowed | `"allow_override"` |
| 4 | Expand `user_roles` → `role_permissions`. If match → allowed | `"role:<role_name>"` |
| 5 | Default → denied | `"default_deny"` |

**Priority rule:** `DENY override > ALLOW override > role-based permission > default deny`

**Active** means: `expires_at IS NULL OR expires_at > NOW()`.

### Go types

```go
// internal/model/model.go
package model

import (
    "errors"
    "time"
    "github.com/google/uuid"
)

type Effect string

const (
    EffectAllow Effect = "ALLOW"
    EffectDeny  Effect = "DENY"
)

var (
    ErrUnknownPermission = errors.New("unknown permission")
    ErrUnknownRole       = errors.New("unknown role")
)

// --- Error response envelope (used by all HTTP handlers) ---

type APIError struct {
    Code    string `json:"code"`
    Message string `json:"message"`
}

type ErrorResponse struct {
    Error APIError `json:"error"`
}

// --- Domain types ---

type Permission struct {
    Action       string    `json:"action"`
    ResourceType string    `json:"resource_type"`
    Description  string    `json:"description"`
    CreatedAt    time.Time `json:"created_at"`
}

type Role struct {
    Name        string    `json:"name"`
    Description string    `json:"description"`
    CreatedAt   time.Time `json:"created_at"`
}

type RolePermission struct {
    RoleName     string    `json:"role_name"`
    Action       string    `json:"action"`
    ResourceType string    `json:"resource_type"`
    GrantedAt    time.Time `json:"granted_at"`
}

type UserRole struct {
    UserID       uuid.UUID         `json:"user_id"`
    RoleName     string            `json:"role_name"`
    ResourceType string            `json:"resource_type"`
    ResourceID   string            `json:"resource_id"`
    Scope        map[string]string `json:"scope"`
    GrantedBy    uuid.UUID         `json:"granted_by"`
    GrantedAt    time.Time         `json:"granted_at"`
    ExpiresAt    *time.Time        `json:"expires_at"`
}

type UserPermissionOverride struct {
    UserID       uuid.UUID         `json:"user_id"`
    Action       string            `json:"action"`
    ResourceType string            `json:"resource_type"`
    ResourceID   string            `json:"resource_id"`
    Scope        map[string]string `json:"scope"`
    Effect       Effect            `json:"effect"`
    Reason       string            `json:"reason"`
    SetBy        uuid.UUID         `json:"set_by"`
    CreatedAt    time.Time         `json:"created_at"`
    ExpiresAt    *time.Time        `json:"expires_at"`
}

// --- Request/Response types ---

type CheckRequest struct {
    UserID       uuid.UUID
    Action       string
    ResourceType string
    ResourceID   string
}

type CheckResult struct {
    Allowed bool   `json:"allowed"`
    Source  string `json:"source"`
}

type ResourcesRequest struct {
    UserID       uuid.UUID
    Action       string
    ResourceType string
    ScopeFilter  map[string]string // nil = no filter
}

type ResourcesResult struct {
    ResourceIDs []string `json:"resource_ids"`
}

type PermissionEntry struct {
    Action       string            `json:"action"`
    ResourceType string            `json:"resource_type"`
    ResourceID   string            `json:"resource_id"`
    Scope        map[string]string `json:"scope"`
    Source       string            `json:"source"`
}

// --- DLQ types ---

type DLQReason string

const (
    DLQReasonPermanent          DLQReason = "permanent"
    DLQReasonTransientExhausted DLQReason = "transient_exhausted"
)

type DLQFailure struct {
    Reason     DLQReason `json:"reason"`
    Error      string    `json:"error"`
    RetryCount int       `json:"retry_count"`
    WorkerID   string    `json:"worker_id"`
}

type DLQSource struct {
    Topic     string `json:"topic"`
    Partition int    `json:"partition"`
    Offset    int64  `json:"offset"`
}

type DLQMessage struct {
    DLQEventID         string    `json:"dlq_event_id"`
    DLQOccurredAt      time.Time `json:"dlq_occurred_at"`
    Failure            DLQFailure `json:"failure"`
    Source             DLQSource  `json:"source"`
    OriginalEventID    string    `json:"original_event_id"`
    OriginalEventType  string    `json:"original_event_type"`
    OriginalOccurredAt time.Time `json:"original_occurred_at"`
    OriginalPayload    []byte    `json:"original_payload"` // base64 by encoding/json
}
```

---

## 5. Configuration

**Depends on:** §2

**Output file:** `internal/config/config.go`

```go
// internal/config/config.go
package config

type Config struct {
    DatabaseURL           string `envconfig:"DATABASE_URL" required:"true"`
    DBMaxConns            int    `envconfig:"DB_MAX_CONNS" default:"20"`
    DBMinConns            int    `envconfig:"DB_MIN_CONNS" default:"2"`
    DBMaxConnIdleSecs     int    `envconfig:"DB_MAX_CONN_IDLE_SECS" default:"300"`
    DBMaxConnLifetimeSecs int    `envconfig:"DB_MAX_CONN_LIFETIME_SECS" default:"3600"`
    DBAcquireTimeoutSecs  int    `envconfig:"DB_ACQUIRE_TIMEOUT_SECS" default:"2"`

    RedisAddr       string `envconfig:"REDIS_ADDR" required:"true"`
    CacheTTLSeconds int    `envconfig:"CACHE_TTL_SECONDS" default:"300"`

    KafkaBrokers string `envconfig:"KAFKA_BROKERS" required:"true"`
    KafkaTopic   string `envconfig:"KAFKA_TOPIC" default:"authz.permission-events"`
    KafkaGroupID string `envconfig:"KAFKA_GROUP_ID" default:"authz-worker"`

    HTTPPort    int `envconfig:"HTTP_PORT" default:"3000"`
    MetricsPort int `envconfig:"METRICS_PORT" default:"3001"`

    AdminMode  bool   `envconfig:"ADMIN_MODE" default:"false"`
    AdminToken string `envconfig:"ADMIN_TOKEN" required:"false"`
}
```

**Startup validation** (in `cmd/api/main.go`, after config load):

```go
if cfg.AdminMode && cfg.AdminToken == "" {
    log.Fatal("ADMIN_TOKEN is required when ADMIN_MODE=true")
}
if cfg.AdminMode && cfg.KafkaBrokers == "" {
    log.Fatal("KAFKA_BROKERS is required when ADMIN_MODE=true")
}
```

> Design note: `DATABASE_URL` always points to a read replica for `authz-api`. The admin API publishes Kafka events only. `authz-worker` uses its own `DATABASE_URL` pointing to the primary. `KafkaBrokers` is used by the worker consumer, the API registry consumer, and (when `AdminMode=true`) the admin API producer.
> Design note: Pool sizing — total connections = `num_api_instances × DBMaxConns + num_worker_instances × worker_DBMaxConns + ~3 headroom`. Must be < PostgreSQL `max_connections`. Good starting point per API instance: `(num_vCPUs × 2) + 1`. Worker: 5–10 connections. If the API fleet exceeds ~10 instances, use PgBouncer in transaction pooling mode.

---

## 6. Database schema and queries

**Depends on:** §2, §4

**Output files:** `migrations/001_initial.up.sql`, `migrations/002_seed.up.sql`, `internal/db/postgres.go`, `internal/db/queries.go`

### Primary key strategy

- **Entity tables** (`roles`, `permissions`): natural key as `UNIQUE`, `SMALLINT` surrogate as `PRIMARY KEY`. Surrogate is FK target in join tables. Never exposed externally.
- **Join/assignment tables** (`role_permissions`, `user_roles`, `user_permission_overrides`): composite PK using `SMALLINT` FKs + lookup columns for compact indexes.
- **External identifiers** (`user_id`, `granted_by`, `set_by`): `UUID`.
- **Generated UUIDs** (`event_log.event_id`): UUID v7 via `uuid.NewV7()`.

### DDL

```sql
-- migrations/001_initial.up.sql

CREATE TABLE roles (
    id          SMALLINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE permissions (
    id            SMALLINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    action        TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    description   TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (action, resource_type)
);

CREATE TABLE role_permissions (
    role_id       SMALLINT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    permission_id SMALLINT NOT NULL REFERENCES permissions(id) ON DELETE CASCADE,
    granted_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (role_id, permission_id)
);

CREATE TABLE user_roles (
    user_id       UUID     NOT NULL,
    role_id       SMALLINT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    resource_type TEXT     NOT NULL,
    resource_id   TEXT     NOT NULL,
    scope         JSONB    NOT NULL DEFAULT '{}',
    granted_by    UUID     NOT NULL,
    granted_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at    TIMESTAMPTZ,
    PRIMARY KEY (user_id, role_id, resource_type, resource_id)
);

-- GIN index for scope @> containment queries (GET /query/v1/resources with scope_filter).
-- jsonb_path_ops: compact, fast for @>, does not support ? operators (not needed).
CREATE INDEX idx_user_roles_scope
    ON user_roles USING GIN (scope jsonb_path_ops);

CREATE TABLE user_permission_overrides (
    user_id       UUID     NOT NULL,
    permission_id SMALLINT NOT NULL REFERENCES permissions(id) ON DELETE CASCADE,
    resource_id   TEXT     NOT NULL,
    scope         JSONB    NOT NULL DEFAULT '{}',
    effect        TEXT     NOT NULL CHECK (effect IN ('ALLOW', 'DENY')),
    reason        TEXT,
    set_by        UUID     NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at    TIMESTAMPTZ,
    PRIMARY KEY (user_id, permission_id, resource_id)
);

CREATE INDEX idx_user_permission_overrides_scope
    ON user_permission_overrides USING GIN (scope jsonb_path_ops);

CREATE TABLE event_log (
    event_id     UUID PRIMARY KEY,
    event_type   TEXT NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

> Design note: `user_roles` PK is `(user_id, role_id, resource_type, resource_id)` but the hot check query filters on `(user_id, resource_type, resource_id)` — skipping `role_id`. PostgreSQL can only use the PK index prefix `(user_id)`, then must scan `role_id` values. A dedicated btree index on `(user_id, resource_type, resource_id)` covering `role_id` would fix this. Deferred — acceptable for launch, monitor p99 cache-miss latency.

### Seed data

```sql
-- migrations/002_seed.up.sql

INSERT INTO roles (name, description) VALUES
    ('viewer', 'Read-only access'),
    ('editor', 'Read and write access'),
    ('admin',  'Full access including delete and management')
ON CONFLICT DO NOTHING;

INSERT INTO permissions (action, resource_type, description) VALUES
    ('read',   'document', 'Read a document'),
    ('write',  'document', 'Write a document'),
    ('delete', 'document', 'Delete a document')
ON CONFLICT DO NOTHING;

INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id FROM roles r, permissions p
WHERE (r.name, p.action, p.resource_type) IN (
    ('viewer', 'read',   'document'),
    ('editor', 'read',   'document'),
    ('editor', 'write',  'document'),
    ('admin',  'read',   'document'),
    ('admin',  'write',  'document'),
    ('admin',  'delete', 'document')
)
ON CONFLICT DO NOTHING;
```

### DB pool setup (`internal/db/postgres.go`)

```go
// internal/db/postgres.go
package db

// NewPool creates a pgxpool.Pool from Config.
// AfterConnect hook: SET default_transaction_read_only = on (for authz-api).
// On startup, verify replica with SELECT pg_is_in_recovery(). If false, log warning.
```

### SQL queries (`internal/db/queries.go`)

Define all queries as named constants. Each query has a comment stating which resolution step or endpoint uses it.

```sql
-- checkOverrideQuery (Steps 2 & 3)
SELECT effect FROM user_permission_overrides
WHERE user_id       = $1   -- UUID
  AND permission_id = $2   -- SMALLINT from registry
  AND resource_id   = $3   -- exact resource ID
  AND (expires_at IS NULL OR expires_at > NOW());

-- checkRoleQuery (Step 4)
SELECT r.name
FROM user_roles ur
JOIN role_permissions rp ON rp.role_id = ur.role_id
JOIN roles r             ON r.id       = ur.role_id
WHERE ur.user_id       = $1
  AND rp.permission_id = $2
  AND ur.resource_type = $3
  AND ur.resource_id   = $4
  AND (ur.expires_at IS NULL OR ur.expires_at > NOW())
LIMIT 1;

-- resourcesQueryNoScope (GET /query/v1/resources — no scope filter)
SELECT upo.resource_id
FROM user_permission_overrides upo
WHERE upo.user_id = $1 AND upo.permission_id = $2 AND upo.effect = 'ALLOW'
  AND (upo.expires_at IS NULL OR upo.expires_at > NOW())
UNION
SELECT ur.resource_id
FROM user_roles ur
JOIN role_permissions rp ON rp.role_id = ur.role_id
WHERE ur.user_id = $1 AND rp.permission_id = $2 AND ur.resource_type = $3
  AND (ur.expires_at IS NULL OR ur.expires_at > NOW())
EXCEPT
SELECT upo.resource_id
FROM user_permission_overrides upo
WHERE upo.user_id = $1 AND upo.permission_id = $2 AND upo.effect = 'DENY'
  AND (upo.expires_at IS NULL OR upo.expires_at > NOW());

-- resourcesQueryWithScope (GET /query/v1/resources — with scope filter)
-- NEVER use ($4::jsonb IS NULL OR scope @> $4). When $4 is NULL, NULL @> NULL
-- evaluates to NULL (falsy) and silently drops all rows. Use two separate queries.
SELECT upo.resource_id
FROM user_permission_overrides upo
WHERE upo.user_id = $1 AND upo.permission_id = $2 AND upo.effect = 'ALLOW'
  AND upo.scope @> $4
  AND (upo.expires_at IS NULL OR upo.expires_at > NOW())
UNION
SELECT ur.resource_id
FROM user_roles ur
JOIN role_permissions rp ON rp.role_id = ur.role_id
WHERE ur.user_id = $1 AND rp.permission_id = $2 AND ur.resource_type = $3
  AND ur.scope @> $4
  AND (ur.expires_at IS NULL OR ur.expires_at > NOW())
EXCEPT
SELECT upo.resource_id
FROM user_permission_overrides upo
WHERE upo.user_id = $1 AND upo.permission_id = $2 AND upo.effect = 'DENY'
  AND (upo.expires_at IS NULL OR upo.expires_at > NOW());
```

---

## 7. Cache layer

**Depends on:** §2, §4

**Output file:** `internal/cache/redis.go`

| Concern | Detail |
| --- | --- |
| Key format | `authz:check:{user_id}:{resource_type}:{resource_id}:{action}` |
| Value | JSON-encoded `CheckResult` |
| TTL | `CACHE_TTL_SECONDS` (default 300) |
| Targeted invalidation | `SCAN` + `DEL` on pattern `authz:check:{user_id}:{resource_type}:{resource_id}:*`. Never use `KEYS`. |
| Full flush | `SCAN` + `DEL` on pattern `authz:check:*`. Never use `FLUSHDB` (Redis may be shared). |

```go
// internal/cache/redis.go
package cache

type Cache struct {
    client *redis.Client
    ttl    time.Duration
}

func NewCache(addr string, ttl time.Duration) *Cache

func (c *Cache) Get(ctx context.Context, req model.CheckRequest) (*model.CheckResult, error)
func (c *Cache) Set(ctx context.Context, req model.CheckRequest, result model.CheckResult) error
func (c *Cache) Invalidate(ctx context.Context, userID, resourceType, resourceID string) error
func (c *Cache) FlushAll(ctx context.Context) error
```

`FlushAll` implementation:

```go
func (c *Cache) FlushAll(ctx context.Context) error {
    pattern := "authz:check:*"
    var cursor uint64
    for {
        keys, nextCursor, err := c.client.Scan(ctx, cursor, pattern, 500).Result()
        if err != nil { return err }
        if len(keys) > 0 {
            if err := c.client.Del(ctx, keys...).Err(); err != nil { return err }
        }
        cursor = nextCursor
        if cursor == 0 { break }
    }
    return nil
}
```

---

## 8. In-memory registry

**Depends on:** §2, §4

**Output file:** `internal/registry/registry.go`

The registry translates natural keys (`action`, `resource_type`, `role_name`) into `SMALLINT` surrogate IDs before any DB query, eliminating subqueries on the hot path.

| Concern | Detail |
| --- | --- |
| Data size | Tens to low hundreds of rows. Safe to hold entirely in memory. |
| Startup | Load all `permissions` and `roles` from DB. Fail fast if unreachable. |
| Thread safety | `sync.RWMutex` — readers don't block each other. |
| Live updates | Worker: inline after DB write. API: via Kafka registry consumer. |
| Unknown key | `Resolve*` returns `(0, false)` → handler returns 404 before any DB query. |

```go
// internal/registry/registry.go
package registry

type Registry struct {
    mu          sync.RWMutex
    permissions map[permKey]int16  // (action, resource_type) → id
    roles       map[string]int16   // role_name → id
    roleNames   map[int16]string   // id → role_name (for source labels)
}

type permKey struct {
    Action       string
    ResourceType string
}

func New(ctx context.Context, q *db.Queries) (*Registry, error)

func (r *Registry) ResolvePermission(action, resourceType string) (int16, bool)
func (r *Registry) ResolveRole(name string) (int16, bool)
func (r *Registry) RoleName(id int16) string

func (r *Registry) AddPermission(action, resourceType string, id int16)
func (r *Registry) RemovePermission(action, resourceType string)
func (r *Registry) AddRole(name string, id int16)
func (r *Registry) RemoveRole(name string)
```

### Registry consumer in authz-api

A background goroutine in `authz-api` consumes `authz.permission-events`. It must **NOT share a consumer group** — each API instance needs all messages. Use `kafka-go`'s `Reader` with explicit partition assignment or a unique group ID per instance (e.g. `authz-api-registry-<hostname>`).

| Event | Registry action |
| --- | --- |
| `action.registered` | No-op (wait for `action.id.assigned`) |
| `action.id.assigned` | `AddPermission(action, resourceType, id)` |
| `action.removed` | `RemovePermission(action, resourceType)` |
| `role.created` | No-op (wait for `role.id.assigned`) |
| `role.id.assigned` | `AddRole(name, id)` |
| `role.removed` | `RemoveRole(name)` |

This goroutine does **not** write to DB and does **not** invalidate Redis. On error: log, skip, continue. Never crash the API process.

---

## 9. Resolver

**Depends on:** §2, §4, §7, §8

**Output files:** `internal/resolver/resolver.go`, `internal/resolver/resolver_test.go`

The resolver implements the 5-step algorithm from §4. It is pure business logic — no HTTP, no Kafka.

```go
// internal/resolver/resolver.go
package resolver

type Resolver struct {
    cache    *cache.Cache
    db       *db.Queries
    registry *registry.Registry
}

func NewResolver(c *cache.Cache, q *db.Queries, reg *registry.Registry) *Resolver

// Check evaluates a single permission request.
// Returns ErrUnknownPermission if (action, resource_type) is not in the registry.
func (r *Resolver) Check(ctx context.Context, req model.CheckRequest) (model.CheckResult, error) {
    // Step 0: registry resolve → permID. If !ok → return ErrUnknownPermission
    // Step 1: cache.Get. If hit → return
    // Step 2: db.CheckOverride(permID) → if DENY → result{Allowed:false, Source:"deny_override"}
    // Step 3: db.CheckOverride(permID) → if ALLOW → result{Allowed:true, Source:"allow_override"}
    // Step 4: db.CheckRole(permID) → if roleName != "" → result{Allowed:true, Source:"role:"+roleName}
    // Step 5: result{Allowed:false, Source:"default_deny"}
    // Always: cache.Set before return
}

// List returns all resolved permissions for a user, optionally filtered by resource.
func (r *Resolver) List(ctx context.Context, userID uuid.UUID, resourceType, resourceID string) ([]model.PermissionEntry, error)

// Resources returns all resource IDs where user has the given action.
func (r *Resolver) Resources(ctx context.Context, req model.ResourcesRequest) (model.ResourcesResult, error)

// Filter narrows a candidate list to the subset the user has the action on.
func (r *Resolver) Filter(ctx context.Context, userID uuid.UUID, action, resourceType string, candidateIDs []string) ([]string, error)
```

---

## 10. API endpoints

**Depends on:** §2, §4, §5, §8, §9, §11

**Output files:** `internal/api/router.go`, `internal/api/middleware.go`, `internal/api/errors.go`, `internal/api/handler_check.go`, `internal/api/handler_permissions.go`, `internal/api/handler_resources.go`, `internal/api/handler_admin.go`, `internal/api/handler_health.go`

**Router:** `github.com/go-chi/chi/v5`

```go
// internal/api/router.go
func NewRouter(resolver *resolver.Resolver, adminHandler *AdminHandler, cfg *config.Config) http.Handler {
    r := chi.NewRouter()
    r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
    r.Mount("/query/v1", queryRouter(resolver))
    if cfg.AdminMode {
        r.Mount("/admin/v1", adminRouter(adminHandler, cfg.AdminToken))
    }
    r.Get("/healthz", handleLiveness)
    r.Get("/readyz",  handleReadiness)
    return r
}
```

### Standard error codes

| HTTP status | Error code | When |
| --- | --- | --- |
| 400 | `invalid_request` | Missing/malformed params, invalid JSON, candidate_ids > 1000 |
| 401 | `missing_token` | `X-Admin-Token` header absent on admin endpoint |
| 403 | `invalid_token` | `X-Admin-Token` does not match config |
| 404 | `unknown_permission` | `(action, resource_type)` not in registry |
| 404 | `unknown_role` | `role_name` not in registry |
| 404 | `not_found` | Route exists but entity not found |
| 409 | `already_exists` | Action or role already in registry |
| 500 | `internal_error` | Unexpected server error |

### Query endpoints

---

#### `GET /query/v1/check`

```none
Method:   GET
Path:     /query/v1/check
Params:   user_id (UUID, required), action (string, required),
          resource_type (string, required), resource_id (string, required)
Auth:     none
Calls:    resolver.Check(ctx, CheckRequest{...})
```

| Condition | Status | Response body |
| --- | --- | --- |
| Missing/malformed param | 400 | `{ "error": { "code": "invalid_request", "message": "..." } }` |
| Unknown permission | 404 | `{ "error": { "code": "unknown_permission", "message": "..." } }` |
| Permission allowed | 200 | `{ "allowed": true, "source": "role:editor" }` |
| Permission denied | 200 | `{ "allowed": false, "source": "deny_override" }` |

---

#### `GET /query/v1/permissions`

```none
Method:   GET
Path:     /query/v1/permissions
Params:   user_id (UUID, required), resource_type (string, optional),
          resource_id (string, optional)
Auth:     none
Calls:    resolver.List(ctx, userID, resourceType, resourceID)
```

| Condition | Status | Response body |
| --- | --- | --- |
| Success | 200 | `{ "permissions": [ { "action": "read", "resource_type": "document", "resource_id": "uuid-123", "scope": {...}, "source": "role:viewer" } ] }` |

---

#### `GET /query/v1/resources`

```none
Method:   GET
Path:     /query/v1/resources
Params:   user_id (UUID, required), action (string, required),
          resource_type (string, required),
          scope_filter (JSON string, optional — flat string→string object)
Auth:     none
Calls:    resolver.Resources(ctx, ResourcesRequest{...})
```

Does not use Redis cache. Two query variants: with and without scope_filter.

| Condition | Status | Response body |
| --- | --- | --- |
| Invalid scope_filter JSON | 400 | `{ "error": { "code": "invalid_request", "message": "scope_filter must be a flat JSON object" } }` |
| Unknown permission | 404 | `{ "error": { "code": "unknown_permission", "message": "..." } }` |
| Success | 200 | `{ "resource_ids": ["baz:111", "baz:222"] }` |

---

#### `POST /query/v1/resources/filter`

```none
Method:   POST
Path:     /query/v1/resources/filter
Body:     { "user_id": "uuid", "action": "get", "resource_type": "baz",
            "candidate_ids": ["baz:111", "baz:222", "baz:999"] }
Auth:     none
Calls:    resolver.Filter(ctx, userID, action, resourceType, candidateIDs)
Limit:    candidate_ids max length 1000
```

| Condition | Status | Response body |
| --- | --- | --- |
| candidate_ids > 1000 | 400 | `{ "error": { "code": "invalid_request", "message": "candidate_ids exceeds limit of 1000" } }` |
| Success | 200 | `{ "allowed_ids": ["baz:111"] }` |

---

### Admin endpoints

All admin endpoints require `X-Admin-Token` header. Return `401` if missing, `403` if invalid.

All mutation endpoints publish a Kafka event and return `202 Accepted`. They **never** write to the database.

GET endpoints read from the replica. They are **eventually consistent** — reflected state lags behind `Kafka → worker → primary → replica` by typically 1–2 seconds.

---

#### `GET /admin/v1/actions`

```none
Method:   GET
Path:     /admin/v1/actions
Auth:     X-Admin-Token
Calls:    db.ListPermissions(ctx) (replica read)
Response: 200 — [{ "action": "read", "resource_type": "document", "description": "..." }]
```

---

#### `POST /admin/v1/actions`

```none
Method:   POST
Path:     /admin/v1/actions
Auth:     X-Admin-Token
Body:     { "action": "pin", "resource_type": "board", "description": "..." }
Validate: (action, resource_type) must NOT exist in registry → 409 if exists
Produce:  action.registered (partition key: "registry")
Response: 202 Accepted
Errors:   400 (validation), 409 (already exists)
```

---

#### `DELETE /admin/v1/actions/{action}/{resource_type}`

```none
Method:   DELETE
Path:     /admin/v1/actions/{action}/{resource_type}
Auth:     X-Admin-Token
Validate: (action, resource_type) must exist in registry → 404 if missing
Produce:  action.removed (partition key: "registry")
Response: 202 Accepted
Errors:   404 (not found)
```

---

#### `GET /admin/v1/roles`

```none
Method:   GET
Path:     /admin/v1/roles
Auth:     X-Admin-Token
Calls:    db.ListRoles(ctx) (replica read)
Response: 200 — [{ "name": "editor", "description": "..." }]
```

---

#### `POST /admin/v1/roles`

```none
Method:   POST
Path:     /admin/v1/roles
Auth:     X-Admin-Token
Body:     { "name": "moderator", "description": "..." }
Validate: name must NOT exist in registry → 409 if exists
Produce:  role.created (partition key: "registry")
Response: 202 Accepted
Errors:   409 (already exists)
```

---

#### `GET /admin/v1/roles/{name}/permissions`

```none
Method:   GET
Path:     /admin/v1/roles/{name}/permissions
Auth:     X-Admin-Token
Validate: role name must exist in registry → 404 if missing
Calls:    db.ListRolePermissions(ctx, roleName) (replica read)
Response: 200 — [{ "action": "read", "resource_type": "document" }]
Errors:   404 (unknown role)
```

---

#### `POST /admin/v1/roles/{name}/permissions`

```none
Method:   POST
Path:     /admin/v1/roles/{name}/permissions
Auth:     X-Admin-Token
Body:     { "action": "pin", "resource_type": "board" }
Validate: role_name must exist in registry → 404
          (action, resource_type) must exist in registry → 404
Produce:  role_permissions.assigned (partition key: "registry")
Response: 202 Accepted
Errors:   404 (unknown role or permission)
```

---

#### `DELETE /admin/v1/roles/{name}/permissions/{action}/{resource_type}`

```none
Method:   DELETE
Path:     /admin/v1/roles/{name}/permissions/{action}/{resource_type}
Auth:     X-Admin-Token
Validate: role_name must exist in registry → 404
          (action, resource_type) must exist in registry → 404
Produce:  role_permissions.removed (partition key: "registry")
Response: 202 Accepted
Errors:   404 (unknown role or permission)
```

---

#### `POST /admin/v1/users/{user_id}/roles`

```none
Method:   POST
Path:     /admin/v1/users/{user_id}/roles
Auth:     X-Admin-Token
Body:     { "role_name": "editor", "resource_type": "baz", "resource_id": "baz:789",
            "scope": { "foo": "foo:abc" }, "granted_by": "uuid", "expires_at": "ISO8601|null" }
Validate: role_name must exist in registry → 404
Produce:  role.assigned (partition key: user_id)
Response: 202 Accepted
Errors:   404 (unknown role)
```

---

#### `DELETE /admin/v1/users/{user_id}/roles/{role_name}/{resource_type}/{resource_id}`

```none
Method:   DELETE
Path:     /admin/v1/users/{user_id}/roles/{role_name}/{resource_type}/{resource_id}
Auth:     X-Admin-Token
Produce:  role.revoked (partition key: user_id)
Response: 202 Accepted
```

> Note: DELETE endpoints have no request body. `revoked_by`/`removed_by` in the Kafka event is set to zero UUID for v1. Proper caller identity propagation will be addressed with OAuth2 service-to-service auth.

---

#### `POST /admin/v1/users/{user_id}/overrides`

```none
Method:   POST
Path:     /admin/v1/users/{user_id}/overrides
Auth:     X-Admin-Token
Body:     { "action": "get", "resource_type": "baz", "resource_id": "baz:789",
            "scope": { "foo": "foo:abc" }, "effect": "ALLOW|DENY",
            "reason": "string", "set_by": "uuid", "expires_at": "ISO8601|null" }
Validate: (action, resource_type) must exist in registry → 404
Produce:  override.set (partition key: user_id)
Response: 202 Accepted
Errors:   404 (unknown permission)
```

---

#### `DELETE /admin/v1/users/{user_id}/overrides/{action}/{resource_type}/{resource_id}`

```none
Method:   DELETE
Path:     /admin/v1/users/{user_id}/overrides/{action}/{resource_type}/{resource_id}
Auth:     X-Admin-Token
Produce:  override.removed (partition key: user_id)
Response: 202 Accepted
```

---

### Health endpoints

Always mounted regardless of `AdminMode`.

#### `GET /healthz`

```none
Response: 200 OK (always, if process is running)
Purpose:  Liveness probe. Never checks dependencies.
```

#### `GET /readyz`

```none
Checks:   DB ping (SELECT 1, 1s timeout)
          Redis PING (1s timeout)
          Registry has ≥1 permission entry
          Kafka producer reachable (AdminMode=true only)
Success:  200 OK
Failure:  503 — { "checks": { "db": "ok", "redis": "failed: ...", "registry": "ok", "kafka": "ok" } }
```

---

## 11. Kafka event schema

**Depends on:** §2, §4

**Output file:** `internal/producer/producer.go` (producer wrapper), event structs in `internal/model/` or inline in handler

**Topic:** `authz.permission-events`
**Dead-letter topic:** `authz.permission-events.dlq`

### Event envelope

All events share this structure:

```json
{
  "event": "<event_type>",
  "event_id": "uuid-v7",
  "occurred_at": "2025-01-01T00:00:00Z",
  ...event-specific fields
}
```

### Partition key rules

| Events | Partition key |
| --- | --- |
| `role.assigned`, `role.revoked`, `override.set`, `override.removed` | `user_id` |
| All others (registry events) | `"registry"` |

### Complete event type reference

| Event type | Producer | Partition key |
| --- | --- | --- |
| `role.assigned` | admin API | user_id |
| `role.revoked` | admin API | user_id |
| `override.set` | admin API | user_id |
| `override.removed` | admin API | user_id |
| `action.registered` | admin API | "registry" |
| `action.removed` | admin API | "registry" |
| `role.created` | admin API | "registry" |
| `role.removed` | admin API | "registry" |
| `role_permissions.assigned` | admin API | "registry" |
| `role_permissions.removed` | admin API | "registry" |
| `action.id.assigned` | worker | "registry" |
| `role.id.assigned` | worker | "registry" |

### Event payloads

Each event below is shown as its complete JSON shape. Generate corresponding Go structs with `json` tags.

```json
// role.assigned
{
  "event": "role.assigned", "event_id": "...", "occurred_at": "...",
  "user_id": "uuid", "role_name": "editor",
  "resource_type": "baz", "resource_id": "baz:789",
  "scope": { "foo": "foo:abc", "bar": "bar:xyz" },
  "granted_by": "uuid", "expires_at": "ISO8601 | null"
}

// role.revoked
{
  "event": "role.revoked", "event_id": "...", "occurred_at": "...",
  "user_id": "uuid", "role_name": "editor",
  "resource_type": "baz", "resource_id": "baz:789",
  "revoked_by": "uuid"
}

// override.set
{
  "event": "override.set", "event_id": "...", "occurred_at": "...",
  "user_id": "uuid", "action": "get",
  "resource_type": "baz", "resource_id": "baz:789",
  "scope": { "foo": "foo:abc", "bar": "bar:xyz" },
  "effect": "ALLOW | DENY",
  "reason": "string", "set_by": "uuid", "expires_at": "ISO8601 | null"
}

// override.removed
{
  "event": "override.removed", "event_id": "...", "occurred_at": "...",
  "user_id": "uuid", "action": "get",
  "resource_type": "baz", "resource_id": "baz:789",
  "removed_by": "uuid"
}

// action.registered
{
  "event": "action.registered", "event_id": "...", "occurred_at": "...",
  "action": "pin", "resource_type": "board",
  "description": "Pin a board to a view",
  "registered_by": "uuid"
}

// action.id.assigned  (worker-only internal event)
{
  "event": "action.id.assigned", "event_id": "...", "occurred_at": "...",
  "action": "pin", "resource_type": "board",
  "id": 42
}

// action.removed
{
  "event": "action.removed", "event_id": "...", "occurred_at": "...",
  "action": "pin", "resource_type": "board",
  "removed_by": "uuid"
}

// role.created
{
  "event": "role.created", "event_id": "...", "occurred_at": "...",
  "role_name": "moderator", "description": "...",
  "created_by": "uuid"
}

// role.id.assigned  (worker-only internal event)
{
  "event": "role.id.assigned", "event_id": "...", "occurred_at": "...",
  "role_name": "moderator",
  "id": 4
}

// role_permissions.assigned
{
  "event": "role_permissions.assigned", "event_id": "...", "occurred_at": "...",
  "role_name": "editor", "action": "pin", "resource_type": "board",
  "assigned_by": "uuid"
}

// role_permissions.removed
{
  "event": "role_permissions.removed", "event_id": "...", "occurred_at": "...",
  "role_name": "editor", "action": "pin", "resource_type": "board",
  "removed_by": "uuid"
}

// role.removed
{
  "event": "role.removed", "event_id": "...", "occurred_at": "...",
  "role_name": "moderator",
  "removed_by": "uuid"
}
```

### DLQ message format

Topic: `authz.permission-events.dlq`
Partition key: `original_event_id`

```json
{
  "dlq_event_id": "uuid-v7",
  "dlq_occurred_at": "2025-01-01T00:00:05Z",
  "failure": {
    "reason": "permanent | transient_exhausted",
    "error": "unknown event type: foo.bar",
    "retry_count": 0,
    "worker_id": "authz-worker-7d9f8b-xkq2p"
  },
  "source": {
    "topic": "authz.permission-events",
    "partition": 3,
    "offset": 198234
  },
  "original_event_id": "uuid-v7-of-original",
  "original_event_type": "role.assigned",
  "original_occurred_at": "2025-01-01T00:00:00Z",
  "original_payload": "<base64-encoded raw kafka message bytes>"
}
```

| `failure.reason` | Meaning | Action |
| --- | --- | --- |
| `permanent` | Rejected immediately — unknown event, bad JSON, FK violation | Manual investigation before replay |
| `transient_exhausted` | Retried max times, still failing | Safe to replay after dependency recovers |

### DLQ replay procedure

- **Transient:** decode `original_payload`, republish to main topic with `original_event_id` as partition key. `event_log` deduplication prevents double-processing.
- **Permanent:** inspect, fix root cause, reconstruct corrected event manually. Do not replay `original_payload`.

---

## 12. Worker (authz-worker)

**Depends on:** §2, §4, §7, §8, §11

**Output files:** `internal/worker/consumer.go`, `internal/worker/handler.go`

**Library:** `github.com/segmentio/kafka-go`

### Consumer groups

| Group | Component | Reads | Writes DB | Writes Redis | Updates registry |
| --- | --- | --- | --- | --- | --- |
| `authz-worker` | authz-worker | All events | Yes (primary) | Yes (invalidation) | Yes (inline) |
| `authz-api-registry-<hostname>` | authz-api | Registry events only | No | No | Yes |

### Event dispatch table

This is the worker's central routing table. Each row maps directly to a handler function.

```go
// internal/worker/handler.go

func (h *Handler) Handle(ctx context.Context, msg kafka.Message) error {
    var env EventEnvelope
    if err := json.Unmarshal(msg.Value, &env); err != nil {
        return h.sendToDLQ(ctx, msg, DLQReasonPermanent, err)
    }

    switch env.Event {
    case "role.assigned":              return h.handleRoleAssigned(ctx, msg, env)
    case "role.revoked":               return h.handleRoleRevoked(ctx, msg, env)
    case "override.set":               return h.handleOverrideSet(ctx, msg, env)
    case "override.removed":           return h.handleOverrideRemoved(ctx, msg, env)
    case "action.registered":          return h.handleActionRegistered(ctx, msg, env)
    case "action.removed":             return h.handleActionRemoved(ctx, msg, env)
    case "action.id.assigned":         return h.handleActionIDAssigned(ctx, msg, env)
    case "role.created":               return h.handleRoleCreated(ctx, msg, env)
    case "role.removed":               return h.handleRoleRemoved(ctx, msg, env)
    case "role.id.assigned":           return h.handleRoleIDAssigned(ctx, msg, env)
    case "role_permissions.assigned":  return h.handleRolePermAssigned(ctx, msg, env)
    case "role_permissions.removed":   return h.handleRolePermRemoved(ctx, msg, env)
    default:
        return h.sendToDLQ(ctx, msg, DLQReasonPermanent, fmt.Errorf("unknown event type: %s", env.Event))
    }
}
```

### Per-event handler specifications

Each handler follows this template:

```none
Event:     <event_type>
DB:        <SQL operation — INSERT/DELETE/UPDATE with ON CONFLICT>
Registry:  <registry method call, or "no-op">
Cache:     <Invalidate(user, resType, resID) | FlushAll | "none">
Publish:   <follow-up event, or "none">
```

---

```none
Event:     role.assigned
DB:        INSERT INTO user_roles (...) ON CONFLICT DO NOTHING
Registry:  no-op
Cache:     Invalidate(user_id, resource_type, resource_id)
Publish:   none
```

```none
Event:     role.revoked
DB:        DELETE FROM user_roles WHERE user_id=$1 AND role_id=$2 AND resource_type=$3 AND resource_id=$4
Registry:  no-op
Cache:     Invalidate(user_id, resource_type, resource_id)
Publish:   none
```

```none
Event:     override.set
DB:        INSERT INTO user_permission_overrides (...) ON CONFLICT DO UPDATE SET effect=$5, reason=$6, set_by=$7, expires_at=$8
Registry:  no-op
Cache:     Invalidate(user_id, resource_type, resource_id)
Publish:   none
```

```none
Event:     override.removed
DB:        DELETE FROM user_permission_overrides WHERE user_id=$1 AND permission_id=$2 AND resource_id=$3
Registry:  no-op
Cache:     Invalidate(user_id, resource_type, resource_id)
Publish:   none
```

```none
Event:     action.registered
DB:        INSERT INTO permissions (action, resource_type, description) ... RETURNING id
Registry:  AddPermission(action, resource_type, id) — inline immediately
Cache:     none
Publish:   action.id.assigned { action, resource_type, id }
```

```none
Event:     action.removed
DB:        DELETE FROM permissions WHERE action=$1 AND resource_type=$2 (CASCADE)
Registry:  RemovePermission(action, resource_type)
Cache:     FlushAll
Publish:   none
```

```none
Event:     action.id.assigned
DB:        none (this is the worker's own follow-up event)
Registry:  AddPermission (idempotent no-op if already present from inline update)
Cache:     none
Publish:   none
```

```none
Event:     role.created
DB:        INSERT INTO roles (name, description) ... RETURNING id
Registry:  AddRole(name, id) — inline immediately
Cache:     none
Publish:   role.id.assigned { role_name, id }
```

```none
Event:     role.removed
DB:        DELETE FROM roles WHERE name=$1 (CASCADE)
Registry:  RemoveRole(name)
Cache:     FlushAll
Publish:   none
```

```none
Event:     role.id.assigned
DB:        none
Registry:  AddRole (idempotent no-op)
Cache:     none
Publish:   none
```

```none
Event:     role_permissions.assigned
DB:        INSERT INTO role_permissions (role_id, permission_id) ... ON CONFLICT DO NOTHING
Registry:  no-op
Cache:     FlushAll
Publish:   none
```

```none
Event:     role_permissions.removed
DB:        DELETE FROM role_permissions WHERE role_id=$1 AND permission_id=$2
Registry:  no-op
Cache:     FlushAll
Publish:   none
```

### Idempotency

All DB writes within a transaction that also inserts `event_id` into `event_log`. Duplicate `event_id` → PK violation → transaction rollback → message safely skipped.

### Error handling and DLQ routing

| Error class | Examples | Action |
| --- | --- | --- |
| Permanent | Unknown event type, unparseable JSON, FK violation | DLQ immediately, reason: `permanent`. Do not retry. Commit offset. |
| Transient | DB timeout, Redis unavailable, Kafka producer timeout | Retry with exponential backoff (100 ms initial, 2× multiplier, max 5 attempts). After exhaustion → DLQ with reason: `transient_exhausted`. Commit offset. |

**DLQ publish is best-effort.** If it fails, log the full `DLQMessage` as structured JSON at ERROR level. Do not block offset commit on a failed DLQ publish.

**Offset commit discipline:** commit only after DB write + cache invalidation + registry update all succeed. For DLQ-routed messages, commit after DLQ publish attempt regardless of success.

### Cache invalidation strategy

| Event type | Invalidation |
| --- | --- |
| `role.assigned`, `role.revoked` | Targeted: `cache.Invalidate(ctx, userID, resourceType, resourceID)` |
| `override.set`, `override.removed` | Targeted: `cache.Invalidate(ctx, userID, resourceType, resourceID)` |
| `role_permissions.assigned`, `role_permissions.removed` | Full flush: `cache.FlushAll(ctx)` |
| `role.removed`, `action.removed` | Full flush: `cache.FlushAll(ctx)` |

> Design note: Full flush is acceptable for role-permission/removal events because they happen at deploy time, not at request time. All instances experience one cache-miss cycle; cache refills within seconds.
> Design note: Multi-worker — inline registry update only covers the worker instance that processed the event. Other instances' registries remain stale until they consume `*.id.assigned`. **Current mitigation:** run a single worker. Future fix: workers also consume `*.id.assigned` from Kafka or look up unknown IDs from DB before DLQ-routing.

---

## 13. Startup and shutdown

**Depends on:** §2, §5, §8, §10, §12

**Output files:** `cmd/api/main.go`, `cmd/worker/main.go`

### `cmd/api/main.go` startup sequence

```none
1. Load config via envconfig
2. Validate AdminMode requirements (AdminToken, KafkaBrokers)
3. Create pgxpool (with AfterConnect read-only hook)
4. Verify replica: SELECT pg_is_in_recovery() — warn if false
5. Create Redis client
6. Load registry from DB — fatal on error
7. Create resolver (cache + db + registry)
8. If AdminMode: create Kafka producer — fatal on connect failure
9. Start registry Kafka consumer (background goroutine, unique group per instance)
10. Create router, start HTTP server
11. Start metrics server on METRICS_PORT
12. Block on SIGTERM/SIGINT
13. Shutdown: drain HTTP, close Kafka consumer, close producer, close pools
```

### `cmd/worker/main.go` startup sequence

```none
1. Load config via envconfig
2. Create pgxpool (primary — NO read-only hook)
3. Create Redis client
4. Load registry from DB — fatal on error
5. Create Kafka producer (for *.id.assigned events)
6. Create worker handler (db + cache + registry + producer)
7. Start Kafka consumer (authz-worker group)
8. Start metrics server on METRICS_PORT
9. Block on SIGTERM/SIGINT
10. Shutdown: stop consuming, drain in-flight, commit offsets, close connections
```

---

## 14. Observability

**Depends on:** §2

### Metrics (Prometheus)

Expose on `METRICS_PORT` (default `9090`) at `/metrics`.

| Metric | Type | Labels |
| --- | --- | --- |
| `authz_check_total` | Counter | `result` (allowed/denied/unknown), `source` |
| `authz_check_duration_seconds` | Histogram | `source` |
| `authz_cache_hit_total` | Counter | — |
| `authz_cache_miss_total` | Counter | — |
| `authz_registry_size` | Gauge | `type` (permissions/roles) |
| `authz_registry_updates_total` | Counter | `event_type` |
| `authz_db_pool_total_conns` | Gauge | — |
| `authz_db_pool_idle_conns` | Gauge | — |
| `authz_db_pool_acquired_conns` | Gauge | — |
| `authz_worker_events_processed_total` | Counter | `event_type`, `status` (ok/dlq) |
| `authz_worker_processing_duration_seconds` | Histogram | `event_type` |

### Tracing

Propagate `X-Request-ID` header. When OpenTelemetry is added, instrument `resolver.Check` and `worker.handleEvent` as spans.

---

## 15. Dependencies (go.mod)

```none
github.com/go-chi/chi/v5
github.com/redis/go-redis/v9
github.com/jackc/pgx/v5
github.com/segmentio/kafka-go
github.com/google/uuid                    // v1.6+ for UUID v7
github.com/kelseyhightower/envconfig
github.com/golang-migrate/migrate/v4
github.com/prometheus/client_golang
```

---

## 16. Migration strategy

Three separate concerns, three separate mechanisms. Do not mix them.

### Track 1 — Schema migrations (`golang-migrate`)

Structural DDL changes. Rare. Applied at startup or via CI.

```none
migrations/
  001_initial.up.sql       # tables + indexes (§6)
  001_initial.down.sql
  002_seed.up.sql          # bootstrap roles + permissions (§6)
  002_seed.down.sql
```

### Track 2 — Operational data changes (Admin API)

New actions, role-permission assignments, role memberships. Always use the Admin API — never raw SQL — so that events are published, audit log is updated, and cache is invalidated.

### Track 3 — Bulk data backfills (standalone Go jobs)

Large-scale mutations in `internal/jobs/`. Requirements: batched (default 500), resumable (cursor-based), idempotent (`ON CONFLICT DO NOTHING`), observable (log every N rows), context-aware (honour cancellation).

```go
// internal/jobs/backfill_example.go
type BackfillJob struct {
    db        *pgx.Pool
    batchSize int
}

func (j *BackfillJob) Run(ctx context.Context) error {
    cursor := ""
    for {
        batch, err := j.fetchBatch(ctx, cursor)
        if err != nil { return err }
        if len(batch) == 0 { return nil }
        if err := j.processBatch(ctx, batch); err != nil { return err }
        cursor = batch[len(batch)-1].cursorKey()
        slog.Info("backfill progress", "cursor", cursor, "batch", len(batch))
        if ctx.Err() != nil { return ctx.Err() }
    }
}
```

---

## 17. Architectural constraints

These are invariants that **must not be violated** by generated code. If any rule below conflicts with an instruction in another section, this section wins.

1. **Scope staleness.** Scope values are written at grant time. If a resource moves in the hierarchy, the granting system must revoke and re-grant. authz-service does not update scope.

2. **No hierarchy traversal.** A grant on `foo:abc` does not imply access to children. Each resource must be granted explicitly.

3. **No wildcards.** There is no `resource_id = "*"`. Every grant references a specific resource instance.

4. **Scope is flat, not typed.** Scope keys are arbitrary strings. authz-service does not validate that keys or values reference real entities.

5. **Role scope is per resource type.** A role may contain permissions for multiple resource types, but a `user_roles` assignment is always scoped to `(resource_type, resource_id)`. Only permissions matching the assignment's `resource_type` are evaluated.

6. **Role-permission change consistency window.** The worker flushes the entire check cache on role-permission and role-removal events. Between publish and flush completion (typically < 2 seconds), cached results may reflect the old permission set.

---

## 18. Project structure

```none
authz-service/
├── cmd/
│   ├── api/
│   │   └── main.go
│   └── worker/
│       └── main.go
├── internal/
│   ├── config/
│   │   └── config.go
│   ├── db/
│   │   ├── postgres.go
│   │   └── queries.go
│   ├── cache/
│   │   └── redis.go
│   ├── registry/
│   │   └── registry.go
│   ├── model/
│   │   └── model.go
│   ├── resolver/
│   │   ├── resolver.go
│   │   └── resolver_test.go
│   ├── api/
│   │   ├── router.go
│   │   ├── middleware.go
│   │   ├── errors.go
│   │   ├── handler_check.go
│   │   ├── handler_permissions.go
│   │   ├── handler_resources.go
│   │   ├── handler_admin.go
│   │   └── handler_health.go
│   ├── producer/
│   │   └── producer.go
│   ├── worker/
│   │   ├── consumer.go
│   │   └── handler.go
│   └── jobs/
│       └── backfill_example.go
├── migrations/
│   ├── 001_initial.up.sql
│   ├── 001_initial.down.sql
│   ├── 002_seed.up.sql
│   └── 002_seed.down.sql
├── Dockerfile
├── docker-compose.yml
└── go.mod
```

---

## 19. Open questions / future additions

- [ ] **gRPC endpoint** — `CheckPermission` RPC for lower-latency inter-service calls.
- [ ] **TTL sweep job** — scheduled cleanup of expired `user_roles` and `user_permission_overrides` rows.
- [ ] **Action namespacing** — prefix actions with team/feature namespace (e.g. `boards:pin`).
- [ ] **Multi-tenancy** — add `tenant_id` to all tables for isolated organizations.
- [ ] **OpenTelemetry** — distributed tracing across API and worker.
- [ ] **Admin API authentication** — replace `X-Admin-Token` with OAuth2.
- [ ] **Resources endpoint pagination** — `limit` + `cursor` for large grant sets.
- [ ] **Resources endpoint caching** — short TTL cache keyed on `(user_id, action, resource_type, scope_filter_hash)`.
- [ ] **Worker crash recovery for `*.id.assigned`** — outbox pattern for exactly-once delivery.
- [ ] **Registry startup race** — second DB reload after Kafka consumer catches up.
- [ ] **`event_log` cleanup** — scheduled `DELETE FROM event_log WHERE processed_at < NOW() - INTERVAL '7 days'`.
