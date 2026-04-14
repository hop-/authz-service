package producer

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
)

type Producer struct {
	writer   *kafka.Writer
	dlqTopic string
}

func NewProducer(brokers string, topic string) *Producer {
	w := &kafka.Writer{
		Addr:         kafka.TCP(strings.Split(brokers, ",")...),
		Topic:        topic,
		Balancer:     &kafka.Hash{},
		RequiredAcks: kafka.RequireAll,
		MaxAttempts:  3,
	}
	return &Producer{
		writer:   w,
		dlqTopic: topic + ".dlq",
	}
}

// Publish sends an event to the main topic with the given partition key.
func (p *Producer) Publish(ctx context.Context, key string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(key),
		Value: data,
	})
}

// PublishDLQ sends a message to the dead-letter topic.
func (p *Producer) PublishDLQ(ctx context.Context, originalEventID string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	dlqWriter := &kafka.Writer{
		Addr:         p.writer.Addr,
		Topic:        p.dlqTopic,
		Balancer:     &kafka.Hash{},
		RequiredAcks: kafka.RequireAll,
		MaxAttempts:  3,
	}
	defer dlqWriter.Close()
	return dlqWriter.WriteMessages(ctx, kafka.Message{
		Key:   []byte(originalEventID),
		Value: data,
	})
}

// PublishRaw sends raw bytes to the main topic with the given partition key.
func (p *Producer) PublishRaw(ctx context.Context, key string, value []byte) error {
	return p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(key),
		Value: value,
	})
}

// NewEventID generates a new UUID v7 for event identification.
func NewEventID() string {
	id, _ := uuid.NewV7()
	return id.String()
}

// NewEventEnvelope creates a base event envelope with common fields.
func NewEventEnvelope(eventType string) map[string]interface{} {
	return map[string]interface{}{
		"event":       eventType,
		"event_id":    NewEventID(),
		"occurred_at": time.Now().UTC().Format(time.RFC3339),
	}
}

// Ping checks if at least one broker is reachable.
func (p *Producer) Ping(ctx context.Context) error {
	// Attempt to resolve the leader for the topic as a connectivity check.
	conn, err := kafka.DialContext(ctx, "tcp", p.writer.Addr.String())
	if err != nil {
		return err
	}
	return conn.Close()
}

// Close closes the underlying writer.
func (p *Producer) Close() error {
	return p.writer.Close()
}
