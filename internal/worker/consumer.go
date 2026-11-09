package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/segmentio/kafka-go"
)

// Fetch error backoff: doubles from fetchInitialBackoff up to fetchMaxBackoff
// and resets after a successful fetch.
const (
	fetchInitialBackoff = 100 * time.Millisecond
	fetchMaxBackoff     = 5 * time.Second
)

// Shutdown drain: after the run context is done, the in-flight message keeps
// being handled for up to DrainTimeout, and its offset commit gets
// commitTimeout of its own.
const (
	DrainTimeout  = 10 * time.Second
	commitTimeout = 5 * time.Second
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
	// sleep waits d between failed fetches and returns ctx.Err() if ctx is
	// done first. Tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
	// drainTimeout is how long the in-flight message may still be handled
	// after shutdown starts. Tests shorten it.
	drainTimeout time.Duration
}

func NewConsumer(brokers []string, topic, groupID string, handler *Handler) *Consumer {
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  brokers,
		Topic:    topic,
		GroupID:  groupID,
		MinBytes: 1,
		MaxBytes: 10e6,
		// Rebalance when partitions are added later, so a grown topic is
		// consumed without a restart. A topic created after the group joined
		// is not reliably picked up, so RunWorker waits for the topics
		// before starting the consumer.
		WatchPartitionChanges: true,
	})
	return newConsumer(reader, handler)
}

func newConsumer(reader messageReader, handler *Handler) *Consumer {
	return &Consumer{reader: reader, handler: handler, sleep: sleepContext, drainTimeout: DrainTimeout}
}

// Run consumes messages until ctx is done, then returns ctx.Err().
//
// Shutdown drains: once ctx is done, Run fetches nothing more, but the
// message already being handled keeps running for up to drainTimeout and is
// committed when it finishes. Past the deadline its handling is cancelled:
// it is neither dead-lettered nor committed, so it is redelivered after
// restart, and Run returns the abort error.
//
// Fetch errors are retried with capped backoff.
func (c *Consumer) Run(ctx context.Context) error {
	delay := fetchInitialBackoff
	for {
		// Stop fetching once shutdown starts: a fetch could still return a
		// buffered message after ctx is done.
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("failed to fetch message", "error", err, "retry_in", delay)
			if err := c.sleep(ctx, delay); err != nil {
				return err
			}
			delay = min(delay*2, fetchMaxBackoff)
			continue
		}
		delay = fetchInitialBackoff

		if err := c.handle(ctx, msg); err != nil {
			messageLogger(ctx, msg).Warn("message handling aborted, offset not committed",
				"error", err,
				"topic", msg.Topic,
				"partition", msg.Partition,
				"offset", msg.Offset,
			)
			return err
		}

		// The commit gets its own short deadline, detached from ctx, so a
		// message drained during shutdown is still committed.
		commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
		err = c.reader.CommitMessages(commitCtx, msg)
		cancel()
		if err != nil {
			messageLogger(ctx, msg).Error("failed to commit message",
				"error", err,
				"topic", msg.Topic,
				"partition", msg.Partition,
				"offset", msg.Offset,
			)
		}
	}
}

// handle runs the handler on a context that survives ctx by up to
// drainTimeout: it is cancelled drainTimeout after ctx is done, or when
// handling finishes.
func (c *Consumer) handle(ctx context.Context, msg kafka.Message) error {
	hctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()

	stop := context.AfterFunc(ctx, func() {
		timer := time.NewTimer(c.drainTimeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			messageLogger(hctx, msg).Warn("drain deadline passed, aborting in-flight event",
				"drain_timeout", c.drainTimeout)
			cancel()
		case <-hctx.Done():
		}
	})
	defer stop()

	return c.handler.Handle(hctx, msg)
}

// Close closes the underlying reader.
func (c *Consumer) Close() error {
	return c.reader.Close()
}
