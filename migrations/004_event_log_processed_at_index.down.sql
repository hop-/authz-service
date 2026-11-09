-- CONCURRENTLY cannot run in a transaction: keep this file to one statement.
DROP INDEX CONCURRENTLY IF EXISTS idx_event_log_processed_at;
