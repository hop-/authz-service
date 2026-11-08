//go:build integration

package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"authz-service/internal/db"
	"authz-service/internal/producer"
	"authz-service/internal/testinfra"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	itPool    *pgxpool.Pool
	itBrokers []string
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	p, stopPG, err := testinfra.StartPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start postgres:", err)
		os.Exit(1)
	}
	b, stopKafka, err := testinfra.StartKafka(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start kafka:", err)
		if err := stopPG(); err != nil {
			fmt.Fprintln(os.Stderr, "stop postgres:", err)
		}
		os.Exit(1)
	}
	itPool, itBrokers = p, b
	code := m.Run()
	for _, stop := range []func() error{stopKafka, stopPG} {
		if err := stop(); err != nil {
			fmt.Fprintln(os.Stderr, "stop container:", err)
		}
	}
	os.Exit(code)
}

const itWait = 60 * time.Second

func itUniq(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

func itPublisher(t *testing.T, topic string) *producer.Producer {
	t.Helper()
	p := producer.NewProducer(strings.Join(itBrokers, ","), topic)
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Logf("close publisher: %v", err)
		}
	})
	return p
}

// publishPermissionID publishes action.id.assigned on the "registry" key, as
// the worker does.
func publishPermissionID(t *testing.T, p *producer.Producer, action string, id int16) {
	t.Helper()
	e := producer.NewEventEnvelope("action.id.assigned")
	e["action"], e["resource_type"], e["id"] = action, "document", id
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.Publish(ctx, "registry", e); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// runSyncer starts s.Run and returns a channel with Run's result. onConsuming
// is closed once the readers sit at the captured offsets.
func runSyncer(t *testing.T, s *Syncer) (consuming <-chan map[int]int64, stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan map[int]int64, 1)
	s.onConsuming = func(offsets map[int]int64) { ch <- offsets }
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	stopped := false
	stop = func() error {
		if stopped {
			return nil
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			return errors.New("Run did not return after cancellation")
		}
	}
	t.Cleanup(func() {
		if err := stop(); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("syncer stop: %v", err)
		}
	})
	return ch, stop
}

func awaitConsuming(t *testing.T, ch <-chan map[int]int64) map[int]int64 {
	t.Helper()
	select {
	case offsets := <-ch:
		return offsets
	case <-time.After(itWait):
		t.Fatal("syncer never started consuming")
		return nil
	}
}

func eventuallyResolves(t *testing.T, reg *Registry, action string, wantID int16) {
	t.Helper()
	deadline := time.Now().Add(itWait)
	for time.Now().Before(deadline) {
		if id, ok := reg.ResolvePermission(action, "document"); ok && id == wantID {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("registry never resolved (%s, document) to %d", action, wantID)
}

func TestSyncer_NoReplayAndPostCaptureApply(t *testing.T) {
	topic := itUniq("sync.events")
	testinfra.CreateTopics(t, itBrokers, topic)
	pub := itPublisher(t, topic)
	q := db.NewQueries(itPool)

	// Published before the syncer starts, for a permission not in the DB:
	// replaying the topic would add it.
	before := itUniq("before")
	publishPermissionID(t, pub, before, 901)

	reg, err := New(context.Background(), q)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	s := NewSyncer(itBrokers, topic, reg, q, time.Hour)
	consuming, stop := runSyncer(t, s)
	offsets := awaitConsuming(t, consuming)
	if len(offsets) != testinfra.TopicPartitions {
		t.Fatalf("captured offsets for %d partitions, want %d: %v", len(offsets), testinfra.TopicPartitions, offsets)
	}
	var captured int64
	for _, o := range offsets {
		captured += o
	}
	if captured != 1 {
		t.Errorf("captured end offsets %v, want the pre-start event counted (sum 1)", offsets)
	}

	after := itUniq("after")
	publishPermissionID(t, pub, after, 902)
	eventuallyResolves(t, reg, after, 902)

	if _, ok := reg.ResolvePermission(before, "document"); ok {
		t.Fatal("an event published before the syncer started was replayed")
	}

	// Removal after capture is applied too.
	removed := producer.NewEventEnvelope("action.removed")
	removed["action"], removed["resource_type"] = after, "document"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := pub.Publish(ctx, "registry", removed); err != nil {
		t.Fatalf("publish: %v", err)
	}
	deadline := time.Now().Add(itWait)
	for {
		if _, ok := reg.ResolvePermission(after, "document"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("action.removed after capture was not applied")
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after cancellation = %v, want context.Canceled", err)
	}
}

func TestSyncer_PeriodicReloadPicksUpDBOnlyChange(t *testing.T) {
	topic := itUniq("sync.reload")
	testinfra.CreateTopics(t, itBrokers, topic)
	q := db.NewQueries(itPool)
	reg, err := New(context.Background(), q)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	s := NewSyncer(itBrokers, topic, reg, q, time.Second)
	consuming, _ := runSyncer(t, s)
	awaitConsuming(t, consuming)

	// A permission written to the DB with no event on the topic.
	action := itUniq("dbonly")
	var id int16
	applied, err := q.ApplyEvent(context.Background(), uuid.New(), "action.registered", func(w db.Writer) error {
		var err error
		id, err = w.InsertPermission(context.Background(), action, "document", "db only")
		return err
	})
	if err != nil || !applied {
		t.Fatalf("insert permission: (%v, %v)", applied, err)
	}

	eventuallyResolves(t, reg, action, id)
}

func TestSyncer_WaitsForTopic(t *testing.T) {
	topic := itUniq("sync.late")
	q := db.NewQueries(itPool)
	reg, err := New(context.Background(), q)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	s := NewSyncer(itBrokers, topic, reg, q, time.Hour)
	consuming, _ := runSyncer(t, s)

	select {
	case <-consuming:
		t.Fatal("syncer started consuming a topic that doesn't exist")
	case <-time.After(3 * time.Second):
	}

	testinfra.CreateTopics(t, itBrokers, topic)
	awaitConsuming(t, consuming)

	pub := itPublisher(t, topic)
	action := itUniq("late")
	publishPermissionID(t, pub, action, 903)
	eventuallyResolves(t, reg, action, 903)
}

func TestSyncer_CancelDuringTopicWait(t *testing.T) {
	q := db.NewQueries(itPool)
	s := NewSyncer(itBrokers, itUniq("sync.never"), NewEmpty(), q, time.Hour)
	_, stop := runSyncer(t, s)
	time.Sleep(500 * time.Millisecond)

	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}
