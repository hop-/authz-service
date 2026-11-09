-- Hot-path index for the check and list queries on user_roles.
-- CONCURRENTLY cannot run in a transaction: keep this file to one statement.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_user_roles_lookup ON user_roles (user_id, resource_type, resource_id);
