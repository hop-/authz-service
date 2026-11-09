# authz-service

Standalone authorization microservice. Stores and evaluates user permissions for arbitrary resources and actions using RBAC with resource-scoped roles and direct user overrides.

## Architecture

| Component | Role |
| --- | --- |
| **authz-api** | HTTP server. Query endpoints (always) + admin endpoints (when `ADMIN_MODE=true`). Reads from DB replica only. Keeps an in-memory registry of roles and permissions in sync with the event topic. |
| **authz-worker** | Kafka consumer. Sole writer to PostgreSQL. Processes all permission mutations, invalidates Redis cache. Scales horizontally. |

### Kafka topics

The services need two topics, `authz.permission-events` (or `KAFKA_TOPIC`) and its dead-letter topic `<topic>.dlq`. **Infra creates them before startup; the services never create topics.**

- `authz-worker` waits for both topics before it starts consuming, logging an error that names the missing ones. Its consumer group watches for partition changes, so partitions added later are picked up without a restart.
- `authz-api` reports a `kafka_topics` check on `/readyz` (503 with `failed: missing <topic>` until the topic exists), and its registry sync waits for the topic.
- In local development, compose's `kafka-init` service creates both topics (3 partitions, replication factor 1), and broker topic auto-creation is disabled to match production.

If the DLQ topic is unavailable when an event must be dead-lettered, the worker retries the DLQ publish with backoff (up to 30 s apart) and does not commit the event's offset. That partition stalls until the publish succeeds: an event is never dropped.

### Scaling

Several `authz-worker` instances can run in the same consumer group (`KAFKA_GROUP_ID`). Registry events (`action.registered`, `role.created`, ...) all use the partition key `registry`, so they land on one partition and have a single writer. Workers keep no in-memory registry: each event resolves its role and permission ids from the primary database inside its own transaction, so user events can go to any worker, and a role or permission that is removed and re-created always resolves to its current id.

### Registry sync (authz-api)

Each `authz-api` pod syncs its registry without a consumer group. At startup it waits for the topic, records the end offset of every partition, loads the registry from the database, and then consumes every partition from the recorded offsets. It never replays the topic, and nothing is missed between the snapshot and the offsets. It also reloads the registry from the database every `REGISTRY_RELOAD_SECONDS` (default 300) as a safety net. The groupless readers don't watch for new partitions; the periodic reload bounds any gap if partitions are added.

Earlier versions created one consumer group per pod, named `authz-api-registry-<hostname>`. They are no longer used and can be deleted:

```bash
kafka-consumer-groups --bootstrap-server <broker> --list | grep '^authz-api-registry-'
kafka-consumer-groups --bootstrap-server <broker> --delete --group authz-api-registry-<hostname>
```

## Quick Start

```bash
# setup .env files
cp .env.dist .env
# (edit .env)
cp .env.dist .env.worker
# (edit .env.worker)
cp .env.dist .env.api
# (edit .env.api)

# Start dependencies (kafka-init creates the Kafka topics)
docker compose up -d
```

## Migrations

Schema migrations live in `migrations/` as [golang-migrate](https://github.com/golang-migrate/migrate) files (`NNN_name.up.sql` / `NNN_name.down.sql`). Always run them against the **primary** database, never a replica.

**With compose**, the `migrate` service applies every pending `up` migration before `api` and `worker` start (`docker compose up -d` runs it).

**Manually**, with the `migrate` CLI (or the `Dockerfile.migrate` image):

```bash
# PRIMARY_DATABASE_URL is the primary's URL, e.g. postgres://authz:authz@localhost:5432/authz?sslmode=disable
migrate -path migrations -database "$PRIMARY_DATABASE_URL" up

# Roll back the last N migrations (here: 004 and 003), or check the current version
migrate -path migrations -database "$PRIMARY_DATABASE_URL" down 2
migrate -path migrations -database "$PRIMARY_DATABASE_URL" version
```

- **`CONCURRENTLY` index migrations** (`003_user_roles_lookup_index`, `004_event_log_processed_at_index`) build their index without blocking writes. `CREATE/DROP INDEX CONCURRENTLY` can't run inside a transaction, so each of these files holds exactly one statement and must never be wrapped in `BEGIN`/`COMMIT` or merged with other statements.
- **A failed concurrent build leaves an `INVALID` index**, and `IF NOT EXISTS` then skips it. Drop it and run the migration again:

  ```sql
  -- find invalid indexes
  SELECT indexrelid::regclass FROM pg_index WHERE NOT indisvalid;
  DROP INDEX CONCURRENTLY IF EXISTS idx_user_roles_lookup;
  ```

  If golang-migrate marked the version dirty, reset it with `migrate ... force <previous version>` before re-running `up`.
- **`002_seed` down** removes only the seeded roles (`viewer`, `editor`, `admin`), permissions (`read`/`write`/`delete` on `document`) and their pairs. Foreign keys cascade: removing a seeded role also removes its user assignments, and removing a seeded permission removes every role pair and override that uses it.

## API

### Query (always active)

```none
GET  /query/v1/check?user_id=<uuid>&action=read&resource_type=document&resource_id=doc:123
GET  /query/v1/permissions?user_id=<uuid>
GET  /query/v1/resources?user_id=<uuid>&action=read&resource_type=document
POST /query/v1/resources/filter
```

#### `GET /query/v1/permissions` semantics

Returns the user's **effective** permissions: exactly the entries `/query/v1/check` would allow.

- One entry per `(action, resource_type, resource_id)`, sorted by `resource_type`, `resource_id`, `action`. An empty result is `[]`.
- A role grants a permission only on assignments whose `resource_type` matches the permission's resource type.
- A key with an active `DENY` override is omitted. `DENY` entries are never returned.
- `source` is `allow_override` for an active `ALLOW` override (it wins over roles, and carries the override's scope), otherwise `role:<name>`. When several roles grant the same key, the lexicographically smallest role name is reported.
- `scope` is the deciding grant's scope, `{}` when it has none.

#### `POST /query/v1/resources/filter` semantics

Returns `allowed_ids`: the candidates (at most 1000) the user has the action on, in the order given. Only the candidates are checked in the database, so the cost grows with the candidate count, not with the user's grants.

### Admin (`ADMIN_MODE=true`, requires `X-Admin-Token` header)

```none
GET|POST|DELETE  /admin/v1/actions
GET|POST         /admin/v1/roles
GET|POST|DELETE  /admin/v1/roles/{name}/permissions
POST|DELETE      /admin/v1/users/{user_id}/roles
POST|DELETE      /admin/v1/users/{user_id}/overrides
```

Admin requests are validated before an event is published; invalid requests get `400 invalid_request` (or `404 unknown_role` / `unknown_permission`) instead of being dead-lettered by the worker:

- JSON bodies are limited to 1 MiB.
- `POST /users/{user_id}/roles` requires `resource_type`, `resource_id`, and `granted_by` (a UUID); `expires_at`, if set, must be in the future.
- `POST /users/{user_id}/overrides` requires `resource_id`, `effect` (`ALLOW` or `DENY`, exactly), and `set_by` (a UUID); `expires_at`, if set, must be in the future.
- `DELETE` of a user role or override returns 404 when the role or permission is not registered.

**Re-assigning a role is a no-op.** Assigning an existing `(user, role, resource_type, resource_id)` again changes nothing. To change its scope or extend `expires_at`, revoke the assignment and assign it again.

### Health

```none
GET /healthz   — liveness (always 200)
GET /readyz    — readiness (checks DB, Redis, registry, and that the Kafka topic exists; in admin mode also Kafka, passing when any broker is reachable)
```

### Swagger / OpenAPI

Auto-generated docs are served at `/swagger/index.html` when the API is running.

To regenerate after code changes:

```bash
go run github.com/swaggo/swag/cmd/swag@v1.16.6 init -g cmd/api/main.go -o docs/ --parseDependency --parseInternal
```

The generated spec files live in `docs/` (`swagger.json`, `swagger.yaml`). These are committed so the API image serves them without needing the `swag` CLI at runtime.

## Observability

### Metrics

Both binaries serve Prometheus metrics on `METRICS_PORT` (default `3001`) at `/metrics`, next to the Go runtime defaults. Every label value comes from a fixed set.

| Metric | Type | Labels | Binary |
| --- | --- | --- | --- |
| `authz_check_total` | counter | `result` (`allowed`, `denied`, `unknown`), `source` | api |
| `authz_check_duration_seconds` | histogram | `source` | api |
| `authz_cache_hit_total`, `authz_cache_miss_total` | counter | — | api |
| `authz_registry_size` | gauge | `type` (`permissions`, `roles`) | api |
| `authz_registry_updates_total` | counter | `event_type` (registry events, `reload` for a full reload) | api |
| `authz_db_pool_total_conns`, `authz_db_pool_idle_conns`, `authz_db_pool_acquired_conns` | gauge | — | api, worker |
| `authz_worker_events_processed_total` | counter | `event_type`, `status` (`ok`, `duplicate`, `dlq`) | worker |
| `authz_worker_processing_duration_seconds` | histogram | `event_type` | worker |
| `authz_event_log_deleted_total` | counter | — | worker |

- `source` is the deciding category: `cache`, `deny_override`, `allow_override`, `role`, `default_deny`, or `unknown_permission` (with `result="unknown"`). It never contains a role name. Checks that fail with a database error are not counted.
- `event_type` is one of the published event types, or `unknown`. Events aborted by shutdown are not counted.

### Logs

Both binaries write JSON logs to stdout. Every entry has `service` (`authz-api` or `authz-worker`).

- **HTTP:** every request is logged with `request_id` (the `X-Request-ID` header, or a generated UUID v7 that is also returned in the response header), `method`, `path`, `status` and `duration_ms`, plus `user_id` once the handler has parsed it.
- **Worker:** every entry about an event has `event_id` and `event_type`, plus `user_id` for per-user events.

### Shutdown

On SIGTERM the worker stops fetching, finishes the event it is processing (up to 10 s), and commits its offset; past 10 s it abandons the event uncommitted, so it is redelivered. Leaving the consumer group takes a few more seconds, so give the container 30 s to stop (compose sets `stop_grace_period: 30s`; Docker's default of 10 s would kill it mid-drain).

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
| `CACHE_TTL_SECONDS` | no | `300` | Maximum Redis cache TTL. A result decided by an expiring role or override is cached at most until that grant expires, and not at all with under 1 s left. |
| `DB_MAX_CONNS` | no | `20` | Max DB pool connections |
| `REGISTRY_RELOAD_SECONDS` | no | `300` | API: interval between registry reloads from the database |
| `DB_ACQUIRE_TIMEOUT_SECS` | no | `2` | Longest wait for a DB pool connection before a query fails |
| `EVENT_LOG_RETENTION_HOURS` | no | `24` | Worker: delete `event_log` rows processed more than this many hours ago (every 15 min, 5000 rows per batch, one worker at a time). `0` disables the cleanup. |

### `event_log` retention and replays

`event_log` records every event id the worker has applied. It is the worker's deduplication marker, committed with each event's changes, not an audit log. The cleanup keeps it to about `EVENT_LOG_RETENTION_HOURS` of events.

**Replay DLQ messages and reset consumer offsets only within the retention window.** An event whose `event_log` row was already deleted is applied again when it is redelivered. If replays can take longer, raise `EVENT_LOG_RETENTION_HOURS` (or set it to `0`).

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
# Unit tests (no Docker needed)
go test ./...

# Integration tests: Postgres, Redis and Kafka in testcontainers (needs Docker)
go test -race -tags integration -count=1 ./...
```

## Known limitations

Both items are deferred to a later plan; they share one fix (a cache-key redesign).

- **C6: replica lag can re-cache a stale result.** The worker invalidates the cache after it commits a change to the primary. A check that reads a lagging replica right after that can cache the pre-change result for up to the cache TTL. Mitigation for now: keep `CACHE_TTL_SECONDS` modest.
- **H7: invalidation cost.** Invalidation finds keys with Redis `SCAN`, so its cost grows with the number of cached keys (O(cached keys)), and role or permission changes scan the whole `authz:check:*` keyspace.

## Docker

```bash
# API image
docker build --target api -t authz-api .

# Worker image
docker build --target worker -t authz-worker .
```
