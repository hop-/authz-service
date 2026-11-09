package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"authz-service/internal/cache"
	"authz-service/internal/config"
	"authz-service/internal/db"
	"authz-service/internal/kafkatopics"
	"authz-service/internal/metrics"
	"authz-service/internal/producer"
	"authz-service/internal/worker"
)

// RunWorker runs authz-worker until ctx is done, or until its metrics server
// or consumer fails. It returns nil after a clean shutdown, including a
// shutdown requested while it still waits for the Kafka topics, and an error
// when startup fails or a component stops unexpectedly.
//
// Startup order: DB pool (primary), Redis, Kafka producer, metrics server,
// event_log cleanup (unless disabled), then wait for the main and DLQ topics
// before the consumer starts.
func RunWorker(ctx context.Context, cfg *config.Config) error {
	slog.Info("starting " + ServiceWorker)
	if cfg.EventLogRetentionHours < 0 {
		return errors.New("EVENT_LOG_RETENTION_HOURS must be 0 (disabled) or positive")
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	pool, err := db.NewPool(runCtx, cfg, false)
	if err != nil {
		return fmt.Errorf("create db pool: %w", err)
	}
	defer pool.Close()
	metrics.SetDBPool(pool)

	redisCache := cache.NewCache(cfg.RedisAddr)
	defer closeLogged("redis client", redisCache.Close)

	// Queries on the primary. The worker keeps no in-memory registry: each
	// event resolves role and permission ids inside its own transaction.
	queries := db.NewQueries(pool, acquireTimeout(cfg))

	// The producer publishes *.id.assigned events and DLQ messages.
	prod := producer.NewProducer(cfg.KafkaBrokers, cfg.KafkaTopic)
	defer closeLogged("kafka producer", prod.Close)

	hostname, err := os.Hostname()
	if err != nil {
		slog.Warn("hostname unavailable, DLQ worker_id is empty", "error", err)
	}
	handler := worker.NewHandler(queries, redisCache, prod, hostname)

	metricsSrv, err := listen("metrics", cfg.MetricsPort, newMetricsHandler())
	if err != nil {
		return err
	}
	errCh := make(chan error, 1)
	metricsSrv.serve(errCh)

	// The event_log cleanup runs on runCtx; RunWorker waits for it.
	var bg sync.WaitGroup
	defer bg.Wait()
	if cfg.EventLogRetentionHours > 0 {
		retention := time.Duration(cfg.EventLogRetentionHours) * time.Hour
		cleanup := worker.NewCleanup(queries, retention)
		bg.Go(func() { cleanup.Run(runCtx) })
	} else {
		slog.Info("event_log cleanup disabled (EVENT_LOG_RETENTION_HOURS=0)")
	}

	brokers := strings.Split(cfg.KafkaBrokers, ",")
	// consume returns only after runCtx is done, unless the consumer fails.
	consumerDone := make(chan error, 1)
	go func() { consumerDone <- consume(runCtx, cfg, brokers, handler) }()

	var runErr error
	select {
	case <-ctx.Done():
		slog.Info("shutdown requested")
		cancel()
		<-consumerDone
	case runErr = <-errCh:
		slog.Error("metrics server failed, shutting down", "error", runErr)
		cancel()
		<-consumerDone
	case runErr = <-consumerDone:
		// nil means ctx was done first and the consumer already stopped.
		if runErr != nil {
			slog.Error("consumer stopped unexpectedly, shutting down", "error", runErr)
		}
		cancel()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer shutdownCancel()
	metricsSrv.shutdown(shutdownCtx)

	slog.Info(ServiceWorker + " stopped")
	return runErr
}

// consume waits for the main and DLQ topics (infra creates them), then runs
// the Kafka consumer until ctx is done, and closes it. It returns nil when
// it stops because ctx is done, including during the topic wait, and an
// error when the consumer stops on its own.
func consume(ctx context.Context, cfg *config.Config, brokers []string, handler *worker.Handler) error {
	topics := []string{cfg.KafkaTopic, cfg.KafkaTopic + ".dlq"}
	if err := kafkatopics.Wait(ctx, brokers, topics...); err != nil {
		slog.Info("shutdown requested while waiting for kafka topics", "topics", topics)
		return nil
	}

	consumer := worker.NewConsumer(brokers, cfg.KafkaTopic, cfg.KafkaGroupID, handler)
	defer closeLogged("kafka consumer", consumer.Close)
	slog.Info("worker consumer starting", "group_id", cfg.KafkaGroupID)
	err := consumer.Run(ctx)
	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("consumer: %w", err)
}
