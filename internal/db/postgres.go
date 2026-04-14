package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"authz-service/internal/config"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool creates a pgxpool.Pool from Config.
// If readOnly is true, sets default_transaction_read_only = on via AfterConnect.
func NewPool(ctx context.Context, cfg *config.Config, readOnly bool) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	poolCfg.MaxConns = int32(cfg.DBMaxConns)
	poolCfg.MinConns = int32(cfg.DBMinConns)
	poolCfg.MaxConnIdleTime = time.Duration(cfg.DBMaxConnIdleSecs) * time.Second
	poolCfg.MaxConnLifetime = time.Duration(cfg.DBMaxConnLifetimeSecs) * time.Second
	poolCfg.HealthCheckPeriod = 30 * time.Second

	if readOnly {
		poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, "SET default_transaction_read_only = on")
			return err
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	return pool, nil
}

// CheckReplica verifies whether the connected database is a replica.
// Logs a warning if it is not (connected to a primary).
func CheckReplica(ctx context.Context, pool *pgxpool.Pool) {
	var inRecovery bool
	err := pool.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery)
	if err != nil {
		slog.Warn("failed to check replica status", "error", err)
		return
	}
	if !inRecovery {
		slog.Warn("DATABASE_URL points to a primary, not a replica")
	}
}
