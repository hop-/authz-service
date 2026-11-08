package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"authz-service/internal/kafkatopics"

	"github.com/segmentio/kafka-go"
)

// Backoff for the syncer's startup sequence and for reader fetch errors.
const (
	syncInitialBackoff = 100 * time.Millisecond
	syncMaxBackoff     = 5 * time.Second
	syncStartMaxDelay  = 30 * time.Second
)

// Syncer keeps one API pod's registry in sync with the event topic, without a
// consumer group:
//
//  1. wait until the topic exists;
//  2. capture the end offset of every partition;
//  3. reload the registry from the DB (a snapshot at least as new as the offsets);
//  4. consume every partition from its captured offset, applying registry events;
//  5. reload from the DB every reloadEvery, as a safety net.
//
// Events between the capture and the snapshot are applied again on top of it.
// Registry adds and removes are idempotent and per-partition ordered, so the
// state converges. Nothing is replayed from the start of the topic, and no
// consumer group is created.
type Syncer struct {
	brokers     []string
	topic       string
	reg         *Registry
	loader      Loader
	reloadEvery time.Duration

	// applyMu serializes event application with the periodic reload. Without
	// it, an event applied while a reload is in flight would be overwritten by
	// the reload's older snapshot; with it, the event waits and is applied on
	// top of the new snapshot.
	applyMu sync.Mutex

	// onConsuming, if set, runs once the readers are positioned at the
	// captured offsets. Tests use it to know when capture is done.
	onConsuming func(offsets map[int]int64)
}

// NewSyncer creates a Syncer for reg. reloadEvery <= 0 disables the periodic
// reload.
func NewSyncer(brokers []string, topic string, reg *Registry, l Loader, reloadEvery time.Duration) *Syncer {
	return &Syncer{
		brokers:     brokers,
		topic:       topic,
		reg:         reg,
		loader:      l,
		reloadEvery: reloadEvery,
	}
}

// Run syncs the registry until ctx is done. It returns ctx.Err() after every
// reader is closed and every goroutine it started has exited. Errors after
// startup are logged and retried, never returned.
func (s *Syncer) Run(ctx context.Context) error {
	if err := kafkatopics.Wait(ctx, s.brokers, s.topic); err != nil {
		return err
	}

	offsets, err := s.captureAndLoad(ctx)
	if err != nil {
		return err
	}

	// Groupless readers don't watch for partition changes. Registry events all
	// share the "registry" key, so they land on one partition; the periodic
	// reload bounds any gap if partitions are added later.
	var wg sync.WaitGroup
	for partition, offset := range offsets {
		reader := kafka.NewReader(kafka.ReaderConfig{
			Brokers:   s.brokers,
			Topic:     s.topic,
			Partition: partition,
			MinBytes:  1,
			MaxBytes:  10e6,
		})
		if err := reader.SetOffset(offset); err != nil {
			// Only fails for group readers; kept as a guard.
			slog.Error("registry sync: set offset", "partition", partition, "offset", offset, "error", err)
			if cerr := reader.Close(); cerr != nil {
				slog.Warn("registry sync: close reader", "partition", partition, "error", cerr)
			}
			continue
		}
		wg.Go(func() { s.consume(ctx, partition, reader) })
	}
	if s.onConsuming != nil {
		s.onConsuming(offsets)
	}
	if s.reloadEvery > 0 {
		wg.Go(func() { s.reloadLoop(ctx) })
	}

	wg.Wait()
	return ctx.Err()
}

// captureAndLoad captures the end offsets, then reloads the registry, retrying
// both with backoff until they succeed or ctx is done. The order matters: a
// snapshot taken after the capture misses nothing before the offsets.
func (s *Syncer) captureAndLoad(ctx context.Context) (map[int]int64, error) {
	delay := syncInitialBackoff
	for {
		offsets, err := s.endOffsets(ctx)
		if err == nil {
			err = s.reg.Reload(ctx, s.loader)
		}
		if err == nil {
			slog.Info("registry sync: snapshot loaded, consuming from captured offsets",
				"topic", s.topic, "offsets", offsets,
				"permissions", s.reg.PermissionCount(), "roles", s.reg.RoleCount())
			return offsets, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		slog.Error("registry sync: startup failed, retrying", "error", err, "retry_in", delay)
		if err := sleepContext(ctx, delay); err != nil {
			return nil, err
		}
		delay = min(delay*2, syncStartMaxDelay)
	}
}

// endOffsets returns the next offset to be written (LastOffset) for every
// partition of the topic.
func (s *Syncer) endOffsets(ctx context.Context) (map[int]int64, error) {
	client := &kafka.Client{Addr: kafka.TCP(s.brokers...)}

	meta, err := client.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{s.topic}})
	if err != nil {
		return nil, fmt.Errorf("kafka metadata: %w", err)
	}
	var requests []kafka.OffsetRequest
	for _, t := range meta.Topics {
		if t.Name != s.topic {
			continue
		}
		if t.Error != nil {
			return nil, fmt.Errorf("kafka metadata for topic %s: %w", s.topic, t.Error)
		}
		for _, p := range t.Partitions {
			requests = append(requests, kafka.LastOffsetOf(p.ID))
		}
	}
	if len(requests) == 0 {
		return nil, fmt.Errorf("topic %s has no partitions", s.topic)
	}

	res, err := client.ListOffsets(ctx, &kafka.ListOffsetsRequest{
		Topics: map[string][]kafka.OffsetRequest{s.topic: requests},
	})
	if err != nil {
		return nil, fmt.Errorf("kafka list offsets: %w", err)
	}
	offsets := make(map[int]int64, len(requests))
	for _, p := range res.Topics[s.topic] {
		if p.Error != nil {
			return nil, fmt.Errorf("kafka list offsets for %s/%d: %w", s.topic, p.Partition, p.Error)
		}
		offsets[p.Partition] = p.LastOffset
	}
	if len(offsets) != len(requests) {
		return nil, fmt.Errorf("kafka list offsets: got %d of %d partitions", len(offsets), len(requests))
	}
	return offsets, nil
}

// consume applies registry events from one partition until ctx is done, then
// closes the reader.
func (s *Syncer) consume(ctx context.Context, partition int, reader *kafka.Reader) {
	defer func() {
		if err := reader.Close(); err != nil {
			slog.Warn("registry sync: close reader", "partition", partition, "error", err)
		}
	}()

	delay := syncInitialBackoff
	for {
		msg, err := reader.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("registry sync: fetch error", "partition", partition, "error", err, "retry_in", delay)
			if err := sleepContext(ctx, delay); err != nil {
				return
			}
			delay = min(delay*2, syncMaxBackoff)
			continue
		}
		delay = syncInitialBackoff
		s.applyMu.Lock()
		applyEvent(s.reg, msg.Value)
		s.applyMu.Unlock()
	}
}

// reloadLoop reloads the registry from the DB every reloadEvery until ctx is
// done. Errors are logged and never stop the loop.
func (s *Syncer) reloadLoop(ctx context.Context) {
	ticker := time.NewTicker(s.reloadEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.applyMu.Lock()
			err := s.reg.Reload(ctx, s.loader)
			s.applyMu.Unlock()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Error("registry sync: periodic reload failed", "error", err)
				continue
			}
			slog.Info("registry sync: periodic reload",
				"permissions", s.reg.PermissionCount(), "roles", s.reg.RoleCount())
		}
	}
}

// applyEvent applies one registry event to reg. Other events are ignored after
// decoding only their "event" field. Malformed registry events are logged and
// skipped.
func applyEvent(reg *Registry, value []byte) {
	var env struct {
		Event string `json:"event"`
	}
	if err := json.Unmarshal(value, &env); err != nil {
		slog.Error("registry sync: unmarshal envelope", "error", err)
		return
	}

	switch env.Event {
	case "action.id.assigned":
		var evt struct {
			Action       string `json:"action"`
			ResourceType string `json:"resource_type"`
			ID           int16  `json:"id"`
		}
		if err := decodeEvent(env.Event, value, &evt); err != nil {
			return
		}
		reg.AddPermission(evt.Action, evt.ResourceType, evt.ID)
		slog.Info("registry: added permission", "action", evt.Action, "resource_type", evt.ResourceType)
	case "action.removed":
		var evt struct {
			Action       string `json:"action"`
			ResourceType string `json:"resource_type"`
		}
		if err := decodeEvent(env.Event, value, &evt); err != nil {
			return
		}
		reg.RemovePermission(evt.Action, evt.ResourceType)
		slog.Info("registry: removed permission", "action", evt.Action, "resource_type", evt.ResourceType)
	case "role.id.assigned":
		var evt struct {
			RoleName string `json:"role_name"`
			ID       int16  `json:"id"`
		}
		if err := decodeEvent(env.Event, value, &evt); err != nil {
			return
		}
		reg.AddRole(evt.RoleName, evt.ID)
		slog.Info("registry: added role", "role_name", evt.RoleName)
	case "role.removed":
		var evt struct {
			RoleName string `json:"role_name"`
		}
		if err := decodeEvent(env.Event, value, &evt); err != nil {
			return
		}
		reg.RemoveRole(evt.RoleName)
		slog.Info("registry: removed role", "role_name", evt.RoleName)
	default:
		// action.registered and role.created carry no id: wait for *.id.assigned.
		// Every other event type is not a registry event.
	}
}

func decodeEvent(eventType string, value []byte, v any) error {
	if err := json.Unmarshal(value, v); err != nil {
		slog.Error("registry sync: unmarshal event", "event_type", eventType, "error", err)
		return err
	}
	return nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
