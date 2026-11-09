-- CONCURRENTLY cannot run in a transaction: keep this file to one statement.
DROP INDEX CONCURRENTLY IF EXISTS idx_user_roles_lookup;
