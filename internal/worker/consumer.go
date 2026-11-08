package worker

import (
	"context"
	"log/slog"

	"github.com/segmentio/kafka-go"
)

// messageReader is the subset of *kafka.Reader the consumer uses.
type messageReader interface {
	FetchMessage(ctx context.Context) (kafka.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

type Consumer struct {
	reader  messageReader
	handler *Handler
}

func NewConsumer(brokers []string, topic, groupID string, handler *Handler) *Consumer {
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  brokers,
		Topic:    topic,
		GroupID:  groupID,
		MinBytes: 1,
		MaxBytes: 10e6,
	})
	return newConsumer(reader, handler)
}

func newConsumer(reader messageReader, handler *Handler) *Consumer {
	return &Consumer{reader: reader, handler: handler}
}

// Run starts consuming messages until the context is cancelled. A message
// whose handling was aborted by cancellation is not committed, so it is
// redelivered after restart.
func (c *Consumer) Run(ctx context.Context) error {
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("failed to fetch message", "error", err)
			continue
		}

		if err := c.handler.Handle(ctx, msg); err != nil {
			slog.Warn("message handling aborted, offset not committed",
				"error", err,
				"topic", msg.Topic,
				"partition", msg.Partition,
				"offset", msg.Offset,
			)
			return err
		}

		if err := c.reader.CommitMessages(ctx, msg); err != nil {
			slog.Error("failed to commit message",
				"error", err,
				"topic", msg.Topic,
				"partition", msg.Partition,
				"offset", msg.Offset,
			)
		}
	}
}

// Close closes the underlying reader.
func (c *Consumer) Close() error {
	return c.reader.Close()
}
