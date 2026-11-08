//go:build integration

// Package testinfra starts throwaway Postgres, Redis and Kafka containers for
// integration tests (build tag "integration"; needs Docker).
//
// Each container comes in two forms:
//   - Postgres(t), Redis(t), Kafka(t) start a container for one test and
//     terminate it with t.Cleanup;
//   - StartPostgres, StartRedis, StartKafka start a container and return a
//     stop function, for a package-wide container shared from TestMain.
package testinfra

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"authz-service/internal/kafkatopics"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
	"github.com/testcontainers/testcontainers-go"
	tckafka "github.com/testcontainers/testcontainers-go/modules/kafka"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// Images match docker-compose.yml where the testcontainers module allows it.
const (
	postgresImage = "postgres:16-alpine"
	redisImage    = "redis:alpine"
	// The kafka module drives the Confluent image's own startup scripts; it is
	// tested against confluent-local, the single-node KRaft build of cp-kafka.
	kafkaImage = "confluentinc/confluent-local:7.9.1"
)

// TopicPartitions is the partition count CreateTopics uses, as kafka-init
// does in docker-compose.yml.
const TopicPartitions = 3

// startTimeout bounds one container start.
const startTimeout = 3 * time.Minute

// --- Postgres ---

// StartPostgres starts Postgres, applies migrations/*.up.sql in lexical order,
// and returns a pool on the primary. stop closes the pool and terminates the
// container.
func StartPostgres(ctx context.Context) (pool *pgxpool.Pool, stop func() error, err error) {
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("authz"),
		tcpostgres.WithUsername("authz"),
		tcpostgres.WithPassword("authz"),
		tcpostgres.BasicWaitStrategies(),
	)
	terminate := func() error { return testcontainers.TerminateContainer(ctr) }
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("start postgres: %w", err), terminate())
	}

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("postgres dsn: %w", err), terminate())
	}
	pool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("postgres pool: %w", err), terminate())
	}
	if err := applyMigrations(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, errors.Join(err, terminate())
	}

	return pool, func() error {
		pool.Close()
		return terminate()
	}, nil
}

// Postgres starts a migrated Postgres for t and returns a pool on it. The pool
// and container are cleaned up with t.Cleanup.
func Postgres(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool, stop, err := StartPostgres(context.Background())
	if err != nil {
		t.Fatalf("testinfra: %v", err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("testinfra: stop postgres: %v", err)
		}
	})
	return pool
}

func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	root, err := ModuleRoot()
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(root, "migrations", "*.up.sql"))
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no migrations found in %s", filepath.Join(root, "migrations"))
	}
	slices.Sort(files)
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read migration: %w", err)
		}
		// No arguments: pgx uses the simple protocol, which runs every
		// statement in the file.
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply migration %s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

// ModuleRoot returns the directory holding go.mod, walking up from the
// working directory (a test's package directory).
func ModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above the working directory")
		}
		dir = parent
	}
}

// --- Redis ---

// StartRedis starts Redis and returns its host:port address. stop terminates
// the container.
func StartRedis(ctx context.Context) (addr string, stop func() error, err error) {
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	ctr, err := tcredis.Run(ctx, redisImage)
	terminate := func() error { return testcontainers.TerminateContainer(ctr) }
	if err != nil {
		return "", nil, errors.Join(fmt.Errorf("start redis: %w", err), terminate())
	}
	addr, err = ctr.Endpoint(ctx, "")
	if err != nil {
		return "", nil, errors.Join(fmt.Errorf("redis endpoint: %w", err), terminate())
	}
	return addr, terminate, nil
}

// Redis starts Redis for t and returns its host:port address. The container
// is cleaned up with t.Cleanup.
func Redis(t testing.TB) string {
	t.Helper()
	addr, stop, err := StartRedis(context.Background())
	if err != nil {
		t.Fatalf("testinfra: %v", err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("testinfra: stop redis: %v", err)
		}
	})
	return addr
}

// --- Kafka ---

// StartKafka starts a single-node Kafka with topic auto-creation disabled, as
// in production, and returns its brokers. stop terminates the container.
func StartKafka(ctx context.Context) (brokers []string, stop func() error, err error) {
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	ctr, err := tckafka.Run(ctx, kafkaImage,
		testcontainers.WithEnv(map[string]string{
			"KAFKA_AUTO_CREATE_TOPICS_ENABLE": "false",
		}),
	)
	terminate := func() error { return testcontainers.TerminateContainer(ctr) }
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("start kafka: %w", err), terminate())
	}
	brokers, err = ctr.Brokers(ctx)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("kafka brokers: %w", err), terminate())
	}
	return brokers, terminate, nil
}

// Kafka starts Kafka for t, with topic auto-creation disabled, and returns
// its brokers. The container is cleaned up with t.Cleanup.
func Kafka(t testing.TB) []string {
	t.Helper()
	brokers, stop, err := StartKafka(context.Background())
	if err != nil {
		t.Fatalf("testinfra: %v", err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("testinfra: stop kafka: %v", err)
		}
	})
	return brokers
}

// CreateTopics creates topics with TopicPartitions partitions and replication
// factor 1, and waits until the broker's metadata lists all of them.
func CreateTopics(t testing.TB, brokers []string, topics ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	configs := make([]kafka.TopicConfig, len(topics))
	for i, topic := range topics {
		configs[i] = kafka.TopicConfig{Topic: topic, NumPartitions: TopicPartitions, ReplicationFactor: 1}
	}
	client := &kafka.Client{Addr: kafka.TCP(brokers...)}
	res, err := client.CreateTopics(ctx, &kafka.CreateTopicsRequest{Topics: configs})
	if err != nil {
		t.Fatalf("testinfra: create topics %v: %v", topics, err)
	}
	for topic, err := range res.Errors {
		if err != nil {
			t.Fatalf("testinfra: create topic %s: %v", topic, err)
		}
	}

	for {
		missing, err := kafkatopics.Missing(ctx, brokers, topics...)
		if err == nil && len(missing) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("testinfra: topics %v not visible after creation (missing %v, last error %v)", topics, missing, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
