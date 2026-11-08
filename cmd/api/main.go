package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"authz-service/internal/api"
	"authz-service/internal/cache"
	"authz-service/internal/config"
	"authz-service/internal/db"
	"authz-service/internal/kafkatopics"
	"authz-service/internal/producer"
	"authz-service/internal/registry"
	"authz-service/internal/resolver"

	_ "authz-service/docs"

	"github.com/hop-/goconfig"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	slog.Info("starting authz-api")

	// 1. Load config
	if err := goconfig.Load(); err != nil {
		log.Fatal("failed to load config: ", err)
	}
	var cfg config.Config
	if err := goconfig.Extract("", &cfg); err != nil {
		log.Fatal("failed to load config: ", err)
	}

	// 2. Validate AdminMode requirements
	if cfg.AdminMode && cfg.AdminToken == "" {
		log.Fatal("ADMIN_TOKEN is required when ADMIN_MODE=true")
	}
	// The registry syncer consumes the event topic in every mode.
	if cfg.KafkaBrokers == "" {
		log.Fatal("KAFKA_BROKERS is required")
	}
	if cfg.RegistryReloadSecs <= 0 {
		log.Fatal("REGISTRY_RELOAD_SECONDS must be positive")
	}
	brokers := strings.Split(cfg.KafkaBrokers, ",")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 3. Create pgxpool (read-only)
	pool, err := db.NewPool(ctx, &cfg, true)
	if err != nil {
		log.Fatal("failed to create db pool: ", err)
	}
	defer pool.Close()

	// 4. Verify replica
	db.CheckReplica(ctx, pool)

	// 5. Create Redis client
	cacheTTL := time.Duration(cfg.CacheTTLSeconds) * time.Second
	redisCache := cache.NewCache(cfg.RedisAddr)
	defer redisCache.Close()

	// 6. Load registry from DB (fail fast if the first load fails)
	queries := db.NewQueries(pool)
	reg, err := registry.New(ctx, queries)
	if err != nil {
		log.Fatal("failed to load registry: ", err)
	}
	slog.Info("registry loaded",
		"permissions", reg.PermissionCount(),
		"roles", reg.RoleCount(),
	)

	// 7. Create resolver
	res := resolver.NewResolver(redisCache, queries, reg, cacheTTL)

	// 8. If AdminMode: create Kafka producer
	var prod *producer.Producer
	if cfg.AdminMode {
		prod = producer.NewProducer(cfg.KafkaBrokers, cfg.KafkaTopic)
		defer prod.Close()
		slog.Info("admin mode enabled, kafka producer created")
	}

	// 9. Start the registry syncer (no consumer group; see registry.Syncer)
	syncer := registry.NewSyncer(brokers, cfg.KafkaTopic, reg, queries,
		time.Duration(cfg.RegistryReloadSecs)*time.Second)
	syncerDone := make(chan struct{})
	go func() {
		defer close(syncerDone)
		if err := syncer.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("registry syncer stopped", "error", err)
		}
	}()

	// 10. Setup health checks
	health := &api.HealthHandler{
		DB:         queries,
		Cache:      redisCache,
		Registry:   reg,
		Topics:     kafkatopics.Checker{Brokers: brokers},
		KafkaTopic: cfg.KafkaTopic,
		AdminMode:  cfg.AdminMode,
	}
	if prod != nil {
		health.Kafka = prod
	}

	// Create admin handler
	var adminHandler *api.AdminHandler
	if cfg.AdminMode {
		adminHandler = api.NewAdminHandler(queries, reg, prod)
	}

	// Create router
	router := api.NewRouter(res, adminHandler, health, &cfg)

	// Start HTTP server
	httpAddr := fmt.Sprintf(":%d", cfg.HTTPPort)
	httpServer := &http.Server{
		Addr:    httpAddr,
		Handler: router,
	}

	go func() {
		slog.Info("http server starting", "addr", httpAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("http server error: ", err)
		}
	}()

	// 11. Start metrics server
	metricsAddr := fmt.Sprintf(":%d", cfg.MetricsPort)
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsServer := &http.Server{
		Addr:    metricsAddr,
		Handler: metricsMux,
	}

	go func() {
		slog.Info("metrics server starting", "addr", metricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("metrics server error: ", err)
		}
	}()

	// 12. Block on SIGTERM/SIGINT
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig)

	// 13. Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("http server shutdown error", "error", err)
	}
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("metrics server shutdown error", "error", err)
	}
	select {
	case <-syncerDone:
	case <-shutdownCtx.Done():
		slog.Error("registry syncer did not stop before the shutdown timeout")
	}

	slog.Info("authz-api stopped")
}
