-- Supports the worker's event_log retention cleanup (processed_at < cutoff).
-- CONCURRENTLY cannot run in a transaction: keep this file to one statement.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_event_log_processed_at ON event_log (processed_at);
