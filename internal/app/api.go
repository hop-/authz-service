package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"authz-service/internal/api"
	"authz-service/internal/cache"
	"authz-service/internal/config"
	"authz-service/internal/db"
	"authz-service/internal/kafkatopics"
	"authz-service/internal/metrics"
	"authz-service/internal/producer"
	"authz-service/internal/registry"
	"authz-service/internal/resolver"
)

// adminKafkaPingTimeout bounds the admin-mode startup check that Kafka is
// reachable.
const adminKafkaPingTimeout = 10 * time.Second

func validateAPIConfig(cfg *config.Config) error {
	if cfg.AdminMode && cfg.AdminToken == "" {
		return errors.New("ADMIN_TOKEN is required when ADMIN_MODE=true")
	}
	// The registry syncer consumes the event topic in every mode.
	if cfg.KafkaBrokers == "" {
		return errors.New("KAFKA_BROKERS is required")
	}
	if cfg.RegistryReloadSecs <= 0 {
		return errors.New("REGISTRY_RELOAD_SECONDS must be positive")
	}
	return nil
}

// RunAPI runs authz-api until ctx is done or a server fails. It returns nil
// after a clean shutdown, and an error when startup fails or a server stops
// unexpectedly.
//
// Startup order: config checks, DB pool (read-only), Redis, registry load
// (fail fast), Kafka producer and reachability check (admin mode), registry
// syncer, then the HTTP and metrics servers.
func RunAPI(ctx context.Context, cfg *config.Config) error {
	slog.Info("starting " + ServiceAPI)
	if err := validateAPIConfig(cfg); err != nil {
		return err
	}
	brokers := strings.Split(cfg.KafkaBrokers, ",")

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	pool, err := db.NewPool(runCtx, cfg, true)
	if err != nil {
		return fmt.Errorf("create db pool: %w", err)
	}
	defer pool.Close()
	metrics.SetDBPool(pool)
	queries := db.NewQueries(pool, acquireTimeout(cfg))
	queries.CheckReplica(runCtx)

	redisCache := cache.NewCache(cfg.RedisAddr)
	defer closeLogged("redis client", redisCache.Close)

	reg, err := registry.New(runCtx, queries)
	if err != nil {
		return fmt.Errorf("load registry: %w", err)
	}
	slog.Info("registry loaded", "permissions", reg.PermissionCount(), "roles", reg.RoleCount())
	metrics.SetRegistry(reg)

	cacheTTL := time.Duration(cfg.CacheTTLSeconds) * time.Second
	res := resolver.NewResolver(redisCache, queries, reg, cacheTTL)

	health := &api.HealthHandler{
		DB:         queries,
		Cache:      redisCache,
		Registry:   reg,
		Topics:     kafkatopics.Checker{Brokers: brokers},
		KafkaTopic: cfg.KafkaTopic,
		AdminMode:  cfg.AdminMode,
	}
	var adminHandler *api.AdminHandler
	if cfg.AdminMode {
		prod := producer.NewProducer(cfg.KafkaBrokers, cfg.KafkaTopic)
		defer closeLogged("kafka producer", prod.Close)
		pingCtx, pingCancel := context.WithTimeout(runCtx, adminKafkaPingTimeout)
		err := prod.Ping(pingCtx)
		pingCancel()
		if err != nil {
			return fmt.Errorf("admin mode: kafka unreachable at startup: %w", err)
		}
		health.Kafka = prod
		adminHandler = api.NewAdminHandler(queries, reg, prod)
		slog.Info("admin mode enabled, kafka producer created")
	}

	httpSrv, err := listen("http", cfg.HTTPPort, api.NewRouter(res, adminHandler, health, cfg))
	if err != nil {
		return err
	}
	metricsSrv, err := listen("metrics", cfg.MetricsPort, newMetricsHandler())
	if err != nil {
		closeLogged("http listener", httpSrv.ln.Close)
		return err
	}

	// The syncer runs on runCtx; RunAPI waits for it before returning.
	syncer := registry.NewSyncer(brokers, cfg.KafkaTopic, reg, queries,
		time.Duration(cfg.RegistryReloadSecs)*time.Second)
	syncerDone := make(chan struct{})
	go func() {
		defer close(syncerDone)
		if err := syncer.Run(runCtx); err != nil && runCtx.Err() == nil {
			slog.Error("registry syncer stopped", "error", err)
		}
	}()

	errCh := make(chan error, 2)
	httpSrv.serve(errCh)
	metricsSrv.serve(errCh)

	var runErr error
	select {
	case <-ctx.Done():
		slog.Info("shutdown requested")
	case runErr = <-errCh:
		slog.Error("server failed, shutting down", "error", runErr)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer shutdownCancel()
	cancel()
	httpSrv.shutdown(shutdownCtx)
	metricsSrv.shutdown(shutdownCtx)
	select {
	case <-syncerDone:
	case <-shutdownCtx.Done():
		slog.Error("registry syncer did not stop before the shutdown timeout")
	}

	slog.Info(ServiceAPI + " stopped")
	return runErr
}

// acquireTimeout is DB_ACQUIRE_TIMEOUT_SECS as a duration.
func acquireTimeout(cfg *config.Config) time.Duration {
	return time.Duration(cfg.DBAcquireTimeoutSecs) * time.Second
}

// closeLogged calls closeFn and logs its error; used for deferred closes.
func closeLogged(what string, closeFn func() error) {
	if err := closeFn(); err != nil {
		slog.Warn("close "+what, "error", err)
	}
}
