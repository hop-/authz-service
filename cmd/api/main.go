package main

import (
	"context"
	"encoding/json"
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
	"authz-service/internal/producer"
	"authz-service/internal/registry"
	"authz-service/internal/resolver"

	_ "authz-service/docs"

	"github.com/hop-/goconfig"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/segmentio/kafka-go"
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
	if cfg.AdminMode && cfg.KafkaBrokers == "" {
		log.Fatal("KAFKA_BROKERS is required when ADMIN_MODE=true")
	}

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
	redisCache := cache.NewCache(cfg.RedisAddr, cacheTTL)
	defer redisCache.Close()

	// 6. Load registry from DB
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
	res := resolver.NewResolver(redisCache, queries, reg)

	// 8. If AdminMode: create Kafka producer
	var prod *producer.Producer
	if cfg.AdminMode {
		prod = producer.NewProducer(cfg.KafkaBrokers, cfg.KafkaTopic)
		defer prod.Close()
		slog.Info("admin mode enabled, kafka producer created")
	}

	// 9. Start registry Kafka consumer (background goroutine)
	if cfg.KafkaBrokers != "" {
		hostname, _ := os.Hostname()
		groupID := "authz-api-registry-" + hostname
		brokers := strings.Split(cfg.KafkaBrokers, ",")

		go func() {
			reader := kafka.NewReader(kafka.ReaderConfig{
				Brokers:  brokers,
				Topic:    cfg.KafkaTopic,
				GroupID:  groupID,
				MinBytes: 1,
				MaxBytes: 10e6,
			})
			defer reader.Close()

			for {
				msg, err := reader.FetchMessage(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					slog.Error("registry consumer: fetch error", "error", err)
					continue
				}

				handleRegistryEvent(reg, msg)

				if err := reader.CommitMessages(ctx, msg); err != nil {
					slog.Error("registry consumer: commit error", "error", err)
				}
			}
		}()
	}

	// 10. Setup health dependencies
	api.HealthDeps.DB = queries
	api.HealthDeps.Cache = redisCache
	api.HealthDeps.Registry = reg
	api.HealthDeps.Producer = prod
	api.HealthDeps.AdminMode = cfg.AdminMode

	// Create admin handler
	var adminHandler *api.AdminHandler
	if cfg.AdminMode {
		adminHandler = api.NewAdminHandler(queries, reg, prod)
	}

	// Create router
	router := api.NewRouter(res, adminHandler, &cfg)

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

	slog.Info("authz-api stopped")
}

func handleRegistryEvent(reg *registry.Registry, msg kafka.Message) {
	var env struct {
		Event string `json:"event"`
	}
	if err := json.Unmarshal(msg.Value, &env); err != nil {
		slog.Error("registry consumer: unmarshal error", "error", err)
		return
	}

	switch env.Event {
	case "action.registered", "role.created":
		// No-op - wait for *.id.assigned
	case "action.id.assigned":
		var evt struct {
			Action       string `json:"action"`
			ResourceType string `json:"resource_type"`
			ID           int16  `json:"id"`
		}
		if err := json.Unmarshal(msg.Value, &evt); err != nil {
			slog.Error("registry consumer: unmarshal action.id.assigned", "error", err)
			return
		}
		reg.AddPermission(evt.Action, evt.ResourceType, evt.ID)
		slog.Info("registry: added permission", "action", evt.Action, "resource_type", evt.ResourceType)
	case "action.removed":
		var evt struct {
			Action       string `json:"action"`
			ResourceType string `json:"resource_type"`
		}
		if err := json.Unmarshal(msg.Value, &evt); err != nil {
			slog.Error("registry consumer: unmarshal action.removed", "error", err)
			return
		}
		reg.RemovePermission(evt.Action, evt.ResourceType)
		slog.Info("registry: removed permission", "action", evt.Action, "resource_type", evt.ResourceType)
	case "role.id.assigned":
		var evt struct {
			RoleName string `json:"role_name"`
			ID       int16  `json:"id"`
		}
		if err := json.Unmarshal(msg.Value, &evt); err != nil {
			slog.Error("registry consumer: unmarshal role.id.assigned", "error", err)
			return
		}
		reg.AddRole(evt.RoleName, evt.ID)
		slog.Info("registry: added role", "role_name", evt.RoleName)
	case "role.removed":
		var evt struct {
			RoleName string `json:"role_name"`
		}
		if err := json.Unmarshal(msg.Value, &evt); err != nil {
			slog.Error("registry consumer: unmarshal role.removed", "error", err)
			return
		}
		reg.RemoveRole(evt.RoleName)
		slog.Info("registry: removed role", "role_name", evt.RoleName)
	default:
		// Ignore other event types
	}
}
