//go:build integration

package testinfra

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"authz-service/internal/kafkatopics"

	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
)

func TestPostgres_AppliesMigrations(t *testing.T) {
	pool := Postgres(t)
	ctx := context.Background()

	var tables []string
	rows, err := pool.Query(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' ORDER BY table_name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	for _, want := range []string{"event_log", "permissions", "role_permissions", "roles", "user_permission_overrides", "user_roles"} {
		if !slices.Contains(tables, want) {
			t.Errorf("table %s missing after migrations (have %v)", want, tables)
		}
	}

	// 002_seed ran after 001_initial (lexical order).
	var roles int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM roles`).Scan(&roles); err != nil {
		t.Fatalf("count roles: %v", err)
	}
	if roles != 3 {
		t.Errorf("seeded roles = %d, want 3", roles)
	}
}

func TestRedis_Reachable(t *testing.T) {
	addr := Redis(t)
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() }) // close error is irrelevant once the test is done
	ctx := context.Background()

	if err := client.Set(ctx, "k", "v", time.Minute).Err(); err != nil {
		t.Fatalf("SET: %v", err)
	}
	if got, err := client.Get(ctx, "k").Result(); err != nil || got != "v" {
		t.Fatalf("GET = (%q, %v)", got, err)
	}
}

func TestKafka_AutoCreateDisabledAndCreateTopics(t *testing.T) {
	brokers := Kafka(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Writing to a missing topic must not create it.
	w := &kafka.Writer{Addr: kafka.TCP(brokers...), Topic: "smoke.absent", MaxAttempts: 1, AllowAutoTopicCreation: false}
	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	if err := w.WriteMessages(wctx, kafka.Message{Value: []byte("x")}); err == nil {
		t.Error("write to a missing topic succeeded")
	}
	wcancel()
	if err := w.Close(); err != nil {
		t.Logf("close writer: %v", err)
	}
	// A metadata request for a missing topic must not create it either.
	missing, err := kafkatopics.Missing(ctx, brokers, "smoke.absent")
	if err != nil {
		t.Fatalf("Missing: %v", err)
	}
	if !slices.Equal(missing, []string{"smoke.absent"}) {
		t.Fatalf("Missing = %v, want [smoke.absent] (broker auto-create must be off)", missing)
	}

	CreateTopics(t, brokers, "smoke.a", "smoke.b")

	missing, err = kafkatopics.Missing(ctx, brokers, "smoke.a", "smoke.b", "smoke.absent")
	if err != nil {
		t.Fatalf("Missing: %v", err)
	}
	if !slices.Equal(missing, []string{"smoke.absent"}) {
		t.Fatalf("Missing after CreateTopics = %v, want [smoke.absent]", missing)
	}

	client := &kafka.Client{Addr: kafka.TCP(brokers...)}
	meta, err := client.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{"smoke.a"}})
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if len(meta.Topics) != 1 || len(meta.Topics[0].Partitions) != TopicPartitions {
		t.Fatalf("smoke.a metadata = %+v, want %d partitions", meta.Topics, TopicPartitions)
	}
}

func TestModuleRoot(t *testing.T) {
	root, err := ModuleRoot()
	if err != nil {
		t.Fatalf("ModuleRoot: %v", err)
	}
	for _, name := range []string{"go.mod", "migrations"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("%s not found in module root %s: %v", name, root, err)
		}
	}
}
