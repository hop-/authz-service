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

	"authz-service/internal/cache"
	"authz-service/internal/config"
	"authz-service/internal/db"
	"authz-service/internal/producer"
	"authz-service/internal/registry"
	"authz-service/internal/worker"

	"github.com/hop-/goconfig"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	slog.Info("starting authz-worker")

	// 1. Load config
	if err := goconfig.Load(); err != nil {
		log.Fatal("failed to load config: ", err)
	}
	var cfg config.Config
	if err := goconfig.Extract("", &cfg); err != nil {
		log.Fatal("failed to load config: ", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 2. Create pgxpool (primary - NO read-only hook)
	pool, err := db.NewPool(ctx, &cfg, false)
	if err != nil {
		log.Fatal("failed to create db pool: ", err)
	}
	defer pool.Close()

	// 3. Create Redis client
	cacheTTL := time.Duration(cfg.CacheTTLSeconds) * time.Second
	redisCache := cache.NewCache(cfg.RedisAddr, cacheTTL)
	defer redisCache.Close()

	// 4. Load registry from DB
	queries := db.NewQueries(pool)
	reg, err := registry.New(ctx, queries)
	if err != nil {
		log.Fatal("failed to load registry: ", err)
	}
	slog.Info("registry loaded",
		"permissions", reg.PermissionCount(),
		"roles", reg.RoleCount(),
	)

	// 5. Create Kafka producer (for *.id.assigned events)
	prod := producer.NewProducer(cfg.KafkaBrokers, cfg.KafkaTopic)
	defer prod.Close()

	// 6. Create worker handler
	hostname, _ := os.Hostname()
	handler := worker.NewHandler(queries, redisCache, reg, prod, hostname)

	// 7. Start Kafka consumer
	brokers := strings.Split(cfg.KafkaBrokers, ",")
	consumer := worker.NewConsumer(brokers, cfg.KafkaTopic, cfg.KafkaGroupID, handler)

	go func() {
		slog.Info("worker consumer starting", "group_id", cfg.KafkaGroupID)
		if err := consumer.Run(ctx); err != nil {
			if ctx.Err() == nil {
				slog.Error("consumer error", "error", err)
			}
		}
	}()

	// 8. Start metrics server
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

	// 9. Block on SIGTERM/SIGINT
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig)

	// 10. Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	cancel()

	if err := consumer.Close(); err != nil {
		slog.Error("consumer close error", "error", err)
	}
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("metrics server shutdown error", "error", err)
	}

	slog.Info("authz-worker stopped")
}
